package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A folder's ADDRESS declarations — `moved` and `removed` in ronja.json —
// evaluated for ONE stack, against that stack's recorded state.
//
// wfdir validates the declarations with no state at all (moves.go); what is
// here is the half that needs the lock: which entries apply to this stack now,
// what they re-key, and which bound paths are left with no file and no
// declaration.
//
// ⚠️ EVALUATION IS IN MEMORY; ONLY AN ACTING PUSH PERSISTS IT. resolveAddresses
// mutates the folder's lock, local baseline and (for a folder with no stack)
// manifest binding in memory, so every later read in the same command — the
// create decisions, the drift guards, the dry-run's units — sees the re-keyed
// state. Writing it back is saveAddresses, and only runPipelinePush,
// runAutomationPush and runPipelinePublish (which writes the lock anyway, once it
// has a draft to publish) call that: `status`, `sync status`, `sync check` and
// `sync apply --dry-run` evaluate and print, and never write. The plan pass of a
// real `sync apply` evaluates on the same *folder its execute pass then pushes
// with, so the push finds the work done, cached, and persists it.
//
// What a move changes is the ADDRESS only. The row's id, its fingerprints, its
// anchors and its recorded NAME travel with it; nothing is sent to the server.
// A table with no recorded name has the file STEM as its name baseline (see
// nameBaseline), so a move carries the OLD stem with the row before the stem
// changes under it — otherwise the new stem would make the row's real name
// read as somebody else's rename.

// addressMove is one re-key a stack's evaluation made, collapsed along a chain:
// [{A,B},{B,C}] against a stack at A reports A → C.
type addressMove struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind is wfdir.AddressTable / AddressMetric / AddressAutomation.
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// addressRemoval is one path a `removed` declaration took out of this stack's
// state. The row is untouched.
type addressRemoval struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// addressOutcome is what one stack's evaluation did.
type addressOutcome struct {
	Moves    []addressMove
	Removals []addressRemoval
	// changed records that the in-memory state was edited, and therefore that an
	// acting command owes a save.
	changed bool
	saved   bool
	// noted records that noteAddresses has said it: `sync apply` pushes and
	// then publishes the same folder on one evaluation, and each would say it.
	noted bool
}

// addressReadError is an evaluation that failed because the folder could not be
// READ, not because the declarations conflict with it — a tree command reports
// the two under different reasons (unreadable vs address_conflict), and only the
// second is something to fix in ronja.json.
type addressReadError struct{ err error }

func (e *addressReadError) Error() string { return e.err.Error() }
func (e *addressReadError) Unwrap() error { return e.err }

// addressFailureReason is the tree commands' reason for a failed evaluation:
// syncReasonUnreadable for a folder that could not be read, and
// syncReasonAddressConflict for everything else, which is a declaration that
// cannot be applied as it stands.
func addressFailureReason(err error) string {
	var unreadable *addressReadError
	if errors.As(err, &unreadable) {
		return syncReasonUnreadable
	}
	return syncReasonAddressConflict
}

// addressOp is one step of the evaluation, in declared order.
type addressOp struct {
	from, to, kind, id string
	remove             bool
}

// addressUnitsOnDisk is every address this folder holds a file for, path →
// wfdir.Address* kind. Empty for the kinds that do not key rows by file.
func (f *folder) addressUnitsOnDisk() (map[string]string, error) {
	out := map[string]string{}
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		enumeration, err := enumerateFolder(f.Root, wfdir.PipelineKind)
		if err != nil {
			return nil, err
		}
		for p := range enumeration.Files {
			out[p] = wfdir.AddressTable
		}
		metrics, err := readMetricFiles(f.Root)
		if err != nil {
			return nil, err
		}
		for _, m := range metrics {
			// A metric file that does not PARSE is still on disk — the rule
			// "the old file is still here" is about the path, not its content.
			// A nested directory is no address at all.
			if m.Alias != "" {
				out[m.Path] = wfdir.AddressMetric
			}
		}
	case wfdir.KindAutomation:
		enumeration, err := wfdir.Enumerate(f.Root, wfdir.AutomationKind)
		if err != nil {
			return nil, err
		}
		for p := range enumeration.Files {
			out[p] = wfdir.AddressAutomation
		}
	}
	return out, nil
}

// boundAddress is the row this stack records at one address, or "".
func (f *folder) boundAddress(kind, p string) string {
	switch kind {
	case wfdir.AddressTable:
		return f.Binding.Tables[p]
	case wfdir.AddressAutomation:
		return f.Binding.Automations[p]
	case wfdir.AddressMetric:
		id, _, _ := f.live(f.State.For(f.Key)).metricSeen(p)
		return id
	}
	return ""
}

// resolveAddresses evaluates this folder's `moved` and `removed` for the
// selected stack, in memory, once per command. See the file comment.
//
// The refusals, all made BEFORE anything is edited, so a refused evaluation
// leaves the folder exactly as it was read:
//
//   - `moved` in a manifest that does not declare format version 5;
//   - a `from` that is still a file in the folder — the declaration says it is
//     gone, and which of the two the row belongs to is not something to guess;
//   - a `removed` path that is still a file — dropping its entry would make the
//     very next push CREATE a second row for it;
//   - an entry whose `from` AND `to` are both bound here: two rows, one address;
//   - a table move whose checks file stayed at the old stem (see
//     refuseChecksLeftBehind).
//
// An entry whose `from` is not bound is a no-op: already applied on this stack
// (the row is at `to`), or never created here at all. That is what makes the
// declarations safe to keep for ever.
func (f *folder) resolveAddresses() (*addressOutcome, error) {
	if f.addresses != nil || f.addressesErr != nil {
		return f.addresses, f.addressesErr
	}
	out, err := f.evaluateAddresses()
	f.addresses, f.addressesErr = out, err
	return out, err
}

func (f *folder) evaluateAddresses() (*addressOutcome, error) {
	out := &addressOutcome{}
	m := f.Manifest
	if !wfdir.KeysRowsByFile(f.Kind) || (len(m.Moved) == 0 && len(m.Removed) == 0) {
		return out, nil
	}
	if err := m.CheckAddressFormat(); err != nil {
		return nil, err
	}
	onDisk, err := f.addressUnitsOnDisk()
	if err != nil {
		return nil, &addressReadError{err: err}
	}

	// The plan, against a SIMULATION of this stack's state: every refusal is
	// found before the first edit.
	sim := map[string]string{}
	simBound := func(kind, p string) string {
		if id, ok := sim[p]; ok {
			return id
		}
		return f.boundAddress(kind, p)
	}
	var ops []addressOp
	for i, entry := range m.Moved {
		kind := wfdir.AddressUnit(f.Kind, entry.From)
		if _, still := onDisk[entry.From]; still {
			return nil, fmt.Errorf("%s declares %s moved to %s (\"moved\" entry %d), and %s is still in this folder — so which of the two files the row belongs to is not something to guess. Delete %s if the rename is done; if %s is a NEW file reusing the path, %s",
				wfdir.ManifestName, entry.From, entry.To, i+1, entry.From, entry.From, entry.From, f.reuseRemedy("the entry", entry.From))
		}
		fromID, toID := simBound(kind, entry.From), simBound(kind, entry.To)
		switch {
		case fromID != "" && toID != "":
			return nil, fmt.Errorf("%s declares %s moved to %s, and on %s BOTH are bound — %s to %s and %s to %s. One address holds one row, so this move cannot be applied: if %s is the row you want, take the entry out and add %q to \"removed\" (its row is left alone); otherwise fix %s by hand",
				wfdir.ManifestName, entry.From, entry.To, f.describeAddressScope(), entry.From, fromID, entry.To, toID,
				entry.To, entry.From, f.addressRecordFile())
		case fromID != "":
			if kind == wfdir.AddressTable {
				if err := f.refuseChecksLeftBehind(i, entry); err != nil {
					return nil, err
				}
			}
			ops = append(ops, addressOp{from: entry.From, to: entry.To, kind: kind, id: fromID})
			sim[entry.To], sim[entry.From] = fromID, ""
		}
	}
	for _, p := range m.Removed {
		kind := wfdir.AddressUnit(f.Kind, p)
		if _, still := onDisk[p]; still {
			return nil, fmt.Errorf("%s declares %s removed, and it is still in this folder. Removing stops this folder managing the row and leaves it alone — and a file with no row behind it is one the next push CREATES. Delete the file, or take it out of \"removed\" if the row is still this file's; if %s is a NEW file reusing the path, %s — taken out sooner, the new file would be pushed over the old row",
				wfdir.ManifestName, p, p, f.reuseRemedy("it from \"removed\"", p))
		}
		if id := simBound(kind, p); id != "" {
			ops = append(ops, addressOp{from: p, kind: kind, id: id, remove: true})
			sim[p] = ""
		}
	}

	for _, op := range ops {
		if op.remove {
			f.dropAddress(op)
			out.Removals = append(out.Removals, addressRemoval{Path: op.from, Kind: op.kind, ID: op.id})
		} else {
			f.rekeyAddress(op)
			out.Moves = appendMove(out.Moves, addressMove{From: op.from, To: op.to, Kind: op.kind, ID: op.id})
		}
		out.changed = true
	}
	return out, nil
}

// refuseChecksLeftBehind refuses a table move whose checks file stayed at the
// old stem: checks/<old>.json still on disk and checks/<new>.json not.
//
// A checks file is named by its table's stem, so it is renamed WITH the table —
// and rekeyAddress re-keys the checks entry on that assumption. Applied with the
// file left behind, the record would sit at a checks path nothing holds, which
// the checks pass reads as a DELETED checks file: every check the folder owns on
// the table becomes an orphan, and the `--prune` the push then suggests SILENCES
// them, while the file still declaring them is refused for naming no .sql file.
// Refused here, before any edit, so the folder is left exactly as it was read.
//
// The checks file is held to where the CHAIN ends, not to this entry's `to`: in
// [{A, B}, {B, C}] the table ends at C, and telling the author to rename
// checks/A.json to checks/B.json would only be refused again at entry 2.
func (f *folder) refuseChecksLeftBehind(i int, entry wfdir.MovedEntry) error {
	end := entry.To
	for _, later := range f.Manifest.Moved[i+1:] {
		if later.From == end {
			end = later.To
		}
	}
	from, to := wfdir.ChecksPath(tableNameFor(entry.From)), wfdir.ChecksPath(tableNameFor(end))
	if from == to {
		return nil
	}
	if !fileExists(filepath.Join(f.Root, filepath.FromSlash(from))) || fileExists(filepath.Join(f.Root, filepath.FromSlash(to))) {
		return nil
	}
	return fmt.Errorf("%s declares %s moved to %s (\"moved\" entry %d), and the table's health checks are still in %s with nothing at %s. A checks file is named by its table's stem and moves with it — rename %s to %s too, then push again",
		wfdir.ManifestName, entry.From, end, i+1, from, to, from, to)
}

// fileExists reports whether a path names something on disk.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// reuseRemedy is wfdir.AddressReuseRemedy for the two refusals that need the
// folder's files to see; wfdir gives the same remedy for a cycle or a
// backwards chain.
func (f *folder) reuseRemedy(what, p string) string {
	return wfdir.AddressReuseRemedy(f.Stack != "", what, p)
}

// appendMove collapses a chain: a move whose `from` is where an earlier move in
// this same evaluation left the same row extends that move rather than adding
// a second one.
func appendMove(moves []addressMove, next addressMove) []addressMove {
	for i := range moves {
		if moves[i].ID == next.ID && moves[i].To == next.From {
			moves[i].To = next.To
			return moves
		}
	}
	return append(moves, next)
}

// rekeyAddress applies one move to every record this folder keeps for the
// selected stack: the lock (or, with no stack, the manifest's binding), the
// local baseline, the in-memory binding — and, for a table, the checks entry
// keyed by its stem, since a check is a child of its table and moves with it.
func (f *folder) rekeyAddress(op addressOp) {
	inst := f.State.For(f.Key)
	paths := [][2]string{{op.from, op.to}}
	if op.kind == wfdir.AddressTable {
		if from, to := wfdir.ChecksPath(tableNameFor(op.from)), wfdir.ChecksPath(tableNameFor(op.to)); from != to {
			paths = append(paths, [2]string{from, to})
		}
	}
	for _, p := range paths {
		if f.Stack != "" {
			f.Lock.RekeyPath(f.Stack, p[0], p[1])
		}
		inst.RekeyPath(p[0], p[1])
	}
	f.editBinding(func(b *wfdir.Binding) {
		switch op.kind {
		case wfdir.AddressTable:
			delete(b.Tables, op.from)
			b.Tables[op.to] = op.id
		case wfdir.AddressAutomation:
			delete(b.Automations, op.from)
			b.Automations[op.to] = op.id
		}
	})
	f.carryNameBaseline(op)
}

// carryNameBaseline carries the OLD address's stem to the new one when nothing
// is recorded there — see the file comment. A recorded name, or a stem an
// earlier move already carried, travelled with the entry and is left exactly as
// it is: a chain A → B → C keeps A's stem.
//
// Carried as the STEM (wfdir.LockTable.Stem), never as a recorded name. The
// rule compares against the two the same way, but a drift refusal says which it
// compared against, and only the stem's wording tells the reader that the
// folder never saw this name on the row — so to declare the current one first.
func (f *folder) carryNameBaseline(op addressOp) {
	switch op.kind {
	case wfdir.AddressTable:
		live := f.live(f.namedBaseline())
		if live.tableName(op.to) == "" && live.carriedTableStem(op.to) == "" {
			live.setTableStem(op.to, op.id, tableNameFor(op.from))
		}
	case wfdir.AddressMetric:
		live := f.live(f.namedBaseline())
		if live.metricName(op.to, op.id) == "" && live.carriedMetricStem(op.to, op.id) == "" {
			stem, _ := wfdir.MetricAlias(op.from)
			live.setMetricStem(op.to, op.id, stem)
		}
	case wfdir.AddressAutomation:
		// A folder with no stack has nowhere to record anything about an
		// automation (the whole anchor layer is off there — see
		// recordAutomationSeen), so its baseline after a move is the NEW
		// filename. Said in the README and the customer guide: a declared name
		// that differs from the old filename then reads as drift on such a
		// folder, and the remedy is to name a stack.
		if f.Stack != "" && f.Lock.AutomationName(f.Stack, op.to) == "" && f.Lock.AutomationStem(f.Stack, op.to) == "" {
			f.Lock.SetAutomationStem(f.Stack, op.to, op.id, automationNameFor(op.from))
		}
	}
}

// namedBaseline is the local baseline a NAME is recorded in when the folder has
// no stack, created in memory if a fresh checkout has none. A stack folder
// records names in the lock, and gets whatever baseline exists (possibly nil,
// which liveHashes never dereferences on that fork).
func (f *folder) namedBaseline() *wfdir.InstanceState {
	if f.Stack != "" {
		return f.State.For(f.Key)
	}
	return pipelineBaseline(f)
}

// dropAddress applies one removal: the entry leaves every record this folder
// keeps for the stack, and nothing is sent anywhere.
func (f *folder) dropAddress(op addressOp) {
	inst := f.State.For(f.Key)
	paths := []string{op.from}
	if op.kind == wfdir.AddressTable {
		// The table's checks stop being managed with it. Left behind, the checks
		// entry would be visited as a deleted checks file, and `--prune` would
		// then SILENCE checks on a row this folder just promised to leave alone.
		paths = append(paths, wfdir.ChecksPath(tableNameFor(op.from)))
	}
	for _, p := range paths {
		if f.Stack != "" {
			f.Lock.DropPath(f.Stack, p)
		}
		inst.DropPath(p)
	}
	f.editBinding(func(b *wfdir.Binding) {
		delete(b.Tables, op.from)
		delete(b.Automations, op.from)
	})
}

// editBinding edits the in-memory binding through fresh maps (the originals may
// be shared with the manifest), and — for a folder with no stack, whose ids
// live in ronja.json's "instances" — writes it back into the manifest, which
// is that folder's lock.
func (f *folder) editBinding(edit func(*wfdir.Binding)) {
	b := f.Binding
	b.Tables = copyMap(b.Tables)
	b.Automations = copyMap(b.Automations)
	edit(&b)
	if len(b.Tables) == 0 {
		b.Tables = nil
	}
	if len(b.Automations) == 0 {
		b.Automations = nil
	}
	f.Binding = b
	if f.Stack == "" && f.Bound {
		f.Manifest.SetBinding(f.Key, b)
	}
}

// copyMap is maps.Clone except that a nil map comes back EMPTY rather than nil:
// editBinding's edit writes into the copy (a move re-keys into it), and a write
// into a nil map panics.
func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// saveAddresses persists what resolveAddresses did, once, for the acting
// commands — the two pushes, and a publish with something to publish. A stack folder writes its lock and local baseline — never ronja.json,
// which a declaration does not change. A folder with no stack writes ronja.json
// too, because that is where its ids live.
//
// ⚠️ THE LOCAL BASELINE IS WRITTEN FIRST, and the order is what makes a failure
// between the two writes heal itself. The binding (ronja.json, or the lock) is
// what the next run's evaluation reads to decide whether a move still applies:
//
//   - baseline written, binding not: the next run still sees the row at the old
//     path and applies the move again. The baseline's re-key is then a no-op
//     (nothing is left at the old path, and an entry at the new one is never
//     overwritten), so the second save completes the first.
//   - the other order, binding written and baseline not: the next run sees the
//     move as applied and never revisits it, so the baseline's fingerprints and
//     recorded name stay at the old path for good — and a folder with no stack
//     keeps its SQL drift anchor there, which disarms the drift guard for the
//     table.
func (f *folder) saveAddresses() error {
	out := f.addresses
	if out == nil || !out.changed || out.saved {
		return nil
	}
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		return err
	}
	if f.Stack == "" {
		if err := f.saveFolder(); err != nil {
			return err
		}
	} else if err := wfdir.SaveLock(f.Root, f.Lock); err != nil {
		return err
	}
	out.saved = true
	f.recordManagedAnchors()
	return nil
}

// noteAddresses prints what an acting push re-keyed, on stderr, beside its
// other notes — the lock changing under a person is something they should see.
func (f *folder) noteAddresses() {
	out := f.addresses
	if out == nil || out.noted {
		return
	}
	out.noted = true
	for _, m := range out.Moves {
		fmt.Fprintf(os.Stderr, "  Note: moved %s → %s (%s %s) in %s; the %s itself is unchanged.\n",
			m.From, m.To, m.Kind, m.ID, f.addressRecordFile(), m.Kind)
	}
	for _, r := range out.Removals {
		fmt.Fprintf(os.Stderr, "  Note: %s is no longer managed here (removed from %s); %s %s is left exactly as it is.\n",
			r.Path, f.addressRecordFile(), r.Kind, r.ID)
	}
}

// addressRecordFile names where this folder's per-file ids live.
func (f *folder) addressRecordFile() string {
	if f.Stack == "" {
		return wfdir.ManifestName
	}
	return fmt.Sprintf("%s (stack %q)", wfdir.LockName, f.Stack)
}

// describeAddressScope names the stack an evaluation ran against.
func (f *folder) describeAddressScope() string {
	if f.Stack == "" {
		return "this organization"
	}
	return fmt.Sprintf("stack %q", f.Stack)
}
