package wfdir

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// A folder's ADDRESS declarations: `moved` and `removed` in ronja.json.
//
// For the two kinds that bind one row per FILE — a pipeline's .sql and
// metrics/<stem>.json files, an automation folder's .json files — the file's
// folder-relative path is the row's address: it is what ronja.lock.json (and,
// for a folder with no stack, ronja.json's "instances" and .ronja/state.json)
// keys the row's id by. Renaming the file therefore changes the address, and
// without a declaration a push reads the old path as a bound file that is gone
// and the new one as a file with no row — a create plus an orphan.
//
//	"moved":   [{"from": "orders.sql", "to": "sales_orders.sql"}],
//	"removed": ["legacy_rollup.sql"]
//
// `moved` re-keys the recorded entry from one path to the other, carrying the
// id and every fingerprint with it; the row itself, NAME INCLUDED, is not
// touched. `removed` drops the entry and leaves the row exactly as it is: the
// folder stops managing it. Neither deletes anything.
//
// Both are evaluated per stack, by that stack's own apply, against that stack's
// own recorded state (see the commands package). What lives HERE is only what
// can be said about the declarations with no state at all: their shape, which
// paths are addresses of this kind, and whether the list contradicts itself.
// That is checked at load, so a malformed declaration refuses every command
// rather than being half-applied by one.

// MovedEntry is one `moved` declaration.
type MovedEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// UnmarshalJSON refuses an entry with any key but "from" and "to".
//
// Strict, unlike the rest of this file's decoding, because a key this build
// does not understand inside a DECLARATION is not forward-compatible data to
// carry: it is a qualifier on what to re-key ("only in stack dev", "this is a
// metric") that an apply ignoring it would get wrong. Refusing is the only
// reading that cannot move the wrong row.
func (e *MovedEntry) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return fmt.Errorf("a \"moved\" entry is an object, {\"from\": \"<path>\", \"to\": \"<path>\"}, and %s is not one", strings.TrimSpace(string(data)))
	}
	for key := range raw {
		if key != "from" && key != "to" {
			return fmt.Errorf("a \"moved\" entry has key %q, and an entry is {\"from\": \"<path>\", \"to\": \"<path>\"} and nothing else — upgrade the CLI if the key is newer than this build (run: ronja update)", key)
		}
	}
	for _, key := range []string{"from", "to"} {
		value, ok := raw[key]
		if !ok {
			return fmt.Errorf("a \"moved\" entry has no %q — an entry is {\"from\": \"<path>\", \"to\": \"<path>\"}", key)
		}
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return fmt.Errorf("a \"moved\" entry's %q is %s, and it has to be a path string", key, strings.TrimSpace(string(value)))
		}
		if key == "from" {
			e.From = s
		} else {
			e.To = s
		}
	}
	return nil
}

// The three kinds of address a declaration can name. A move never crosses
// them: a table's recorded state is no answer for a metric.
const (
	AddressTable      = "table"
	AddressMetric     = "metric"
	AddressAutomation = "automation"
)

// AddressUnit is the kind of row a folder-relative path addresses in a folder
// of this kind, or "" when the path is not an address at all.
//
// The same rules the loops read their files by: a pipeline's .sql files (any
// depth, the kind's SyncExt), its FLAT metrics/<stem>.json files, and an
// automation folder's .json files. Checks files and docs sidecars are not
// addresses: a check is a child of its table and moves with it, and a sidecar
// documents a row the folder does not build.
func AddressUnit(kind Kind, p string) string {
	if NotSyncable(p, kind) != "" && !(kind.Name == KindPipeline && isMetricPath(p)) {
		return ""
	}
	switch kind.Name {
	case KindPipeline:
		if isMetricPath(p) {
			return AddressMetric
		}
		return AddressTable
	case KindAutomation:
		return AddressAutomation
	}
	return ""
}

func isMetricPath(p string) bool {
	_, ok := MetricAlias(p)
	return ok
}

// KeysRowsByFile reports whether a folder kind binds one row per file, which is
// the only kind of folder a `moved` or `removed` declaration means anything in.
func KeysRowsByFile(kind Kind) bool {
	return kind.Name == KindPipeline || kind.Name == KindAutomation
}

// checkAddressDeclarations validates `moved` and `removed` with no state at all.
//
// Everything refused here is a declaration that contradicts ITSELF, so no stack
// could apply it: a path that is not an address of this kind, a move onto
// itself or across kinds, two entries moving one path (or onto one path), a
// cycle, a chain listed backwards, a path both moved FROM and removed. A cycle
// and a backwards chain are what REUSING a path looks like too, so both
// refusals end with the one remedy for that (reuseRemedy). (A move's
// destination may be removed: see the loop below.) The refusals
// that depend on state — both ends already bound, the old file still on disk —
// belong to each stack's apply.
//
// ⚠️ A CHAIN LISTED BACKWARDS is refused rather than resolved, because entries
// apply in DECLARED ORDER. [{B, C}, {A, B}] against a stack still at A runs the
// first entry as a no-op (nothing is at B yet) and the second as A → B, leaving
// the row at B while the file is at C — and rename detection would then send
// the author to add a `moved` entry they already have. Appending is how these
// lists grow, so the backwards order is a hand-edit worth one sentence.
func (m *Manifest) checkAddressDeclarations(manifestPath string) error {
	if len(m.Moved) == 0 && len(m.Removed) == 0 {
		return nil
	}
	kind, known := kindByName[m.Kind]
	if !known || !KeysRowsByFile(kind) {
		return fmt.Errorf("%s declares \"moved\" or \"removed\", and those re-key the per-file rows of a pipeline or automation folder. A %s folder is ONE resource keyed by the folder itself, so moving or renaming the folder is all a rename takes — remove the key",
			manifestPath, m.Kind)
	}
	unitOf := func(field, p string) (string, error) {
		if err := checkAddressPath(p); err != nil {
			return "", fmt.Errorf("%s: %s names %q, which %v", manifestPath, field, p, err)
		}
		unit := AddressUnit(kind, p)
		if unit == "" {
			want := "a .json automation file"
			if kind.Name == KindPipeline {
				want = "a .sql file or a " + MetricPath("<name>") + " file"
			}
			return "", fmt.Errorf("%s: %s names %q, which is not a file this %s folder builds a row from — it names %s, by its path relative to the folder (the key %s records it under)",
				manifestPath, field, p, kind.Label, want, LockName)
		}
		return unit, nil
	}

	fromAt := map[string]int{}
	toAt := map[string]int{}
	for i, entry := range m.Moved {
		field := fmt.Sprintf("\"moved\" entry %d", i+1)
		fromUnit, err := unitOf(field, entry.From)
		if err != nil {
			return err
		}
		toUnit, err := unitOf(field, entry.To)
		if err != nil {
			return err
		}
		if entry.From == entry.To {
			return fmt.Errorf("%s: %s moves %q onto itself", manifestPath, field, entry.From)
		}
		if fromUnit != toUnit {
			return fmt.Errorf("%s: %s moves a %s file (%s) to a %s file (%s), and a move changes only a row's address — it cannot turn one kind of row into another",
				manifestPath, field, fromUnit, entry.From, toUnit, entry.To)
		}
		if j, dup := fromAt[entry.From]; dup {
			return fmt.Errorf("%s: \"moved\" entries %d and %d both move %q, so which address it has now is not something this file says", manifestPath, j+1, i+1, entry.From)
		}
		if j, dup := toAt[entry.To]; dup {
			return fmt.Errorf("%s: \"moved\" entries %d and %d both move a file to %q, and one address holds one row", manifestPath, j+1, i+1, entry.To)
		}
		fromAt[entry.From], toAt[entry.To] = i, i
	}
	// Cycles first, for the clearer sentence: a cycle also reads as a backwards
	// chain, and naming the loop is the more useful of the two.
	// In declared order, so the loop a refusal names is the same one every run.
	for i, first := range m.Moved {
		loop := []string{first.From}
		for at := first.From; len(loop) <= len(m.Moved); {
			next := m.Moved[fromAt[at]].To
			if next == first.From {
				return fmt.Errorf("%s: \"moved\" declares a cycle (%s → %s): a later entry moves a file back onto a path an earlier one moved away. That reads exactly like a swap of two files, which one address per row cannot express, so it is refused. If you renamed a file back or reused a path, %s",
					manifestPath, strings.Join(loop, " → "), first.From, m.reuseRemedy(i, first.From))
			}
			if _, continues := fromAt[next]; !continues {
				break
			}
			loop = append(loop, next)
			at = next
		}
	}
	for i, entry := range m.Moved {
		if j, ok := toAt[entry.From]; ok && j > i {
			return fmt.Errorf("%s: \"moved\" entry %d moves %q away, and entry %d — LATER in the list — is the one that moves a file to it. Entries apply in the order they are listed, so this is refused whichever it means: if the two renames are one chain, list them in the order they happened (entry %d first); if entry %d reuses the path for a different file, %s",
				manifestPath, i+1, entry.From, j+1, j+1, j+1, m.reuseRemedy(i, entry.From))
		}
	}

	seen := map[string]bool{}
	for i, p := range m.Removed {
		field := fmt.Sprintf("\"removed\" entry %d", i+1)
		if _, err := unitOf(field, p); err != nil {
			return err
		}
		if seen[p] {
			return fmt.Errorf("%s: \"removed\" names %q twice", manifestPath, p)
		}
		seen[p] = true
		if _, moved := fromAt[p]; moved {
			return fmt.Errorf("%s: %q is both moved (to %q) and removed, and a row either keeps being managed at a new address or stops being managed — not both. To retire a renamed file, remove its NEW path (%q) and keep the move",
				manifestPath, p, m.Moved[fromAt[p]].To, m.Moved[fromAt[p]].To)
		}
		// A move's DESTINATION may be removed, and that is the rename-then-retire
		// lifecycle: orders.sql was moved to sales.sql, and later sales.sql is
		// dropped. Removals evaluate after every move, so a stack still at
		// orders.sql is moved and then dropped, one already at sales.sql is
		// dropped — and deleting the `moved` entry instead would leave the lagging
		// stack at a path that is gone, which rename detection refuses.
	}
	return nil
}

// AddressReuseRemedy is the one way out of a declaration that reuses a path:
// the declaration (`what`, already worded: "entry 1", "the entry") has to go,
// and it may go only once nothing still binds the OLD row at `path` — a stack
// still at the path would otherwise read the new file as its old row. A folder
// that names stacks keeps its rows in ronja.lock.json; one that names none
// keeps them in ronja.json, so that is the file to watch there. `path` is
// printed as given, so the caller chooses its quoting.
//
// ⚠️ The entries are otherwise safe to keep for ever, and this is the one
// exception: a path an entry names is the entry's, until the entry is deleted.
func AddressReuseRemedy(stacked bool, what, path string) string {
	if !stacked {
		return fmt.Sprintf("delete %s once %s no longer binds %s", what, ManifestName, path)
	}
	return fmt.Sprintf("delete %s once no stack in %s still binds %s", what, LockName, path)
}

// reuseRemedy is AddressReuseRemedy for the entry at index i of this manifest.
func (m *Manifest) reuseRemedy(i int, p string) string {
	return AddressReuseRemedy(len(m.Stacks) > 0, fmt.Sprintf("entry %d", i+1), strconv.Quote(p))
}

// checkAddressPath refuses a path that is not in the one spelling the lock keys
// rows by: folder-relative, slash-separated, already clean.
//
// Exact rather than normalised, because the declaration is compared against map
// keys byte for byte. Accepting "./orders.sql" and quietly cleaning it would be
// a second spelling of an address, and the next reader comparing the file to
// the lock would see two different paths for one row.
func checkAddressPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("is empty")
	case strings.TrimSpace(p) != p:
		return fmt.Errorf("has leading or trailing whitespace")
	case strings.Contains(p, `\`):
		return fmt.Errorf("uses a backslash; paths here are slash-separated, as %s keys them", LockName)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("is absolute; a path here is relative to the folder")
	case path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("is not in its plain form (no \"./\", \"..\", or doubled slashes) — write it exactly as %s keys it", LockName)
	}
	return nil
}

// CheckAddressFormat refuses `moved` — or `removed` in an automation folder —
// in a manifest that declares a format below the version that means it: the
// stamp a person adding the key by hand will not have written. See
// ManifestFormatVersion.
//
// Not at load, for the reason CheckDeclarations gives about its own version
// rule: `status` must still open the folder whose author needs it explained.
// Every command that APPLIES the declaration asks this first.
func (m *Manifest) CheckAddressFormat() error {
	if m.FormatVersion >= manifestVersionMoves {
		return nil
	}
	if len(m.Moved) == 0 {
		if len(m.Removed) == 0 || m.Kind != KindAutomation {
			return nil
		}
		return fmt.Errorf("%s declares \"removed\" but says \"formatVersion\": %d. A CLI older than version %d of this format ignores the key, refuses the automation's file as gone, and tells the reader to run `ronja automation push --prune` — which pauses and deletes the automation \"removed\" says to keep running — so the file has to say it needs a newer one. Add \"formatVersion\": %d to %s",
			ManifestName, declaredFormatVersion(m.FormatVersion), manifestVersionMoves, manifestVersionMoves, ManifestName)
	}
	return fmt.Errorf("%s declares \"moved\" but says \"formatVersion\": %d. A CLI older than version %d of this format ignores the key, reads the renamed file as new, and CREATES A SECOND row beside the one this folder already has — so the file has to say it needs a newer one. Add \"formatVersion\": %d to %s",
		ManifestName, declaredFormatVersion(m.FormatVersion), manifestVersionMoves, manifestVersionMoves, ManifestName)
}

// rekey moves one map entry from one key to another, refusing to overwrite.
// It reports whether it moved anything.
func rekey[V any](m map[string]V, from, to string) bool {
	value, ok := m[from]
	if !ok {
		return false
	}
	if _, taken := m[to]; taken {
		return false
	}
	delete(m, from)
	m[to] = value
	return true
}

func drop[V any](m map[string]V, key string) bool {
	if _, ok := m[key]; !ok {
		return false
	}
	delete(m, key)
	return true
}

// RekeyPath moves everything one stack records under a path to another path —
// table, metric, automation, checks and docs entries alike, each with its id,
// fingerprints, anchors and recorded name. An entry already at `to` is never
// overwritten. It reports whether anything moved.
//
// One generic move rather than one per map, because "carry everything with it"
// is the whole contract: a per-map version is how a field added to one entry
// later gets left behind at the old address.
func (l *Lock) RekeyPath(stack, from, to string) bool {
	entry, ok := l.entry(stack)
	if !ok {
		return false
	}
	moved := rekey(entry.Tables, from, to)
	moved = rekey(entry.Automations, from, to) || moved
	moved = rekey(entry.TableDocs, from, to) || moved
	moved = rekey(entry.Metrics, from, to) || moved
	moved = rekey(entry.Checks, from, to) || moved
	l.Stacks[stack] = entry
	return moved
}

// DropPath forgets everything one stack records under a path. The ROW is not
// touched — this is the lock losing track of it, which is what `removed` asks
// for. It reports whether anything was dropped.
func (l *Lock) DropPath(stack, path string) bool {
	entry, ok := l.entry(stack)
	if !ok {
		return false
	}
	dropped := drop(entry.Tables, path)
	dropped = drop(entry.Automations, path) || dropped
	dropped = drop(entry.TableDocs, path) || dropped
	dropped = drop(entry.Metrics, path) || dropped
	dropped = drop(entry.Checks, path) || dropped
	l.Stacks[stack] = entry
	return dropped
}

// MetricPaths lists the metric files one stack records a metric id for,
// sorted. It is how a metric whose FILE is gone is still visited: nothing else
// iterates the lock's metrics, so a deleted metric file used to orphan its row
// in silence.
func (l *Lock) MetricPaths(stack string) []string {
	if l == nil {
		return nil
	}
	var out []string
	for p, m := range l.Stacks[stack].Metrics {
		if m.MetricID != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// RekeyPath is Lock.RekeyPath for the local baseline: every map keyed by path
// moves its entry, and none is overwritten.
func (i *InstanceState) RekeyPath(from, to string) bool {
	if i == nil {
		return false
	}
	moved := rekey(i.Files, from, to)
	moved = rekey(i.Tables, from, to) || moved
	moved = rekey(i.TableDocs, from, to) || moved
	moved = rekey(i.Metrics, from, to) || moved
	moved = rekey(i.Checks, from, to) || moved
	moved = rekey(i.StagedNames, from, to) || moved
	return moved
}

// DropPath is Lock.DropPath for the local baseline.
func (i *InstanceState) DropPath(path string) bool {
	if i == nil {
		return false
	}
	dropped := drop(i.Files, path)
	dropped = drop(i.Tables, path) || dropped
	dropped = drop(i.TableDocs, path) || dropped
	dropped = drop(i.Metrics, path) || dropped
	dropped = drop(i.Checks, path) || dropped
	dropped = drop(i.StagedNames, path) || dropped
	return dropped
}
