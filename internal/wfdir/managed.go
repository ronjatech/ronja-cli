package wfdir

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// A folder's ID and its MANAGED state.
//
// A folder with an `"id"` in ronja.json is ADOPTED: the server keeps its state
// (backend resource/rmanaged, `managed_resources`), keyed by (the stack's
// feature, this id, the folder-relative address), and the lock becomes a CACHE
// of it. A folder with no id is LEGACY and runs exactly as it always did, off
// the lock alone. The id is the whole switch between the two patterns, and only
// a deliberate local command writes one: `init` for a new folder and `ronja
// sync adopt` for an existing one. Never `clone`, never a push, never `sync
// apply` — an id minted on a CI runner and lost with its checkout would make the
// next run mint another and duplicate every row.
//
// What lives here is the part that needs no server: the id's shape, and the
// translation between a lock entry and the two anchors the server stores — the
// per-address ANCHOR in a claim's `applied`, and the per-folder one in the
// folder anchor's (FolderAnchor). The commands package does the reading and
// writing.

// folderIDRE is the canonical text form of a folder id, which is the server's
// (rmanaged.NormalizeFolderID): lowercase hex, 8-4-4-4-12. A folder id is
// compared byte for byte in a unique index, so two spellings of one UUID would
// be two addresses.
var folderIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NormalizeFolderID lowercases and validates a folder id.
func NormalizeFolderID(s string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(s))
	if !folderIDRE.MatchString(id) {
		return "", fmt.Errorf("%q is not a folder id — a folder id is a UUID (8-4-4-4-12 hex digits), written by `init` or `ronja sync adopt`", s)
	}
	return id, nil
}

// NewFolderID mints a random (version 4) folder id.
func NewFolderID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint a folder id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Adopted reports whether this folder has an id — whether its state lives on
// the server. Read this, never the field, so "adopted" has one definition.
func (m *Manifest) Adopted() bool { return m.ID != "" }

// The resource kinds a claim names, spelled as the server spells them
// (rmanaged.Kind). A pipeline folder holds two of them.
const (
	ManagedWorkflow   = "workflow"
	ManagedDataApp    = "dataapp"
	ManagedModule     = "module"
	ManagedAutomation = "automation"
	ManagedTable      = "table"
	ManagedMetric     = "metric"
)

// ManagedKindAt is the claim kind a folder of this kind holds at one address,
// or "" when that address is not one this folder kind can hold. A folder kind
// (workflow, data app, module) holds exactly one row, at the empty address.
func ManagedKindAt(kind Kind, address string) string {
	switch kind.Name {
	case KindWorkflow, KindDataApp, KindModule:
		if address != "" {
			return ""
		}
		switch kind.Name {
		case KindWorkflow:
			return ManagedWorkflow
		case KindDataApp:
			return ManagedDataApp
		default:
			return ManagedModule
		}
	case KindPipeline:
		if isMetricPath(address) {
			return ManagedMetric
		}
		if strings.HasSuffix(address, ".sql") {
			return ManagedTable
		}
	case KindAutomation:
		if strings.HasSuffix(address, ".json") {
			return ManagedAutomation
		}
	}
	return ""
}

// ManagedAddress is one row a stack's lock records, at its address.
type ManagedAddress struct {
	Kind    string
	Address string
	ID      string
}

// ManagedAddresses lists every row one stack's lock records, in address order:
// the one row of a folder kind (address ""), or a pipeline's tables and metrics,
// or an automation folder's automations. Checks and docs sidecars are not
// claimable and never appear.
func (l *Lock) ManagedAddresses(stack string, kind Kind) []ManagedAddress {
	if l == nil {
		return nil
	}
	entry := l.Stacks[stack]
	var out []ManagedAddress
	switch kind.Name {
	case KindWorkflow:
		if entry.WorkflowID != "" {
			out = append(out, ManagedAddress{Kind: ManagedWorkflow, ID: entry.WorkflowID})
		}
	case KindDataApp:
		if entry.DataAppID != "" {
			out = append(out, ManagedAddress{Kind: ManagedDataApp, ID: entry.DataAppID})
		}
	case KindModule:
		if entry.ModuleID != "" {
			out = append(out, ManagedAddress{Kind: ManagedModule, ID: entry.ModuleID})
		}
	case KindPipeline:
		for p, t := range entry.Tables {
			if t.TableID != "" {
				out = append(out, ManagedAddress{Kind: ManagedTable, Address: p, ID: t.TableID})
			}
		}
		for p, m := range entry.Metrics {
			if m.MetricID != "" {
				out = append(out, ManagedAddress{Kind: ManagedMetric, Address: p, ID: m.MetricID})
			}
		}
	case KindAutomation:
		for p, a := range entry.Automations {
			if a.AutomationID != "" {
				out = append(out, ManagedAddress{Kind: ManagedAutomation, Address: p, ID: a.AutomationID})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// ManagedID is the row one stack's lock records at one address, or "".
func (l *Lock) ManagedID(stack string, kind Kind, address string) string {
	for _, a := range l.ManagedAddresses(stack, kind) {
		if a.Address == address {
			return a.ID
		}
	}
	return ""
}

// The ANCHOR of each kind: every field a lock entry carries about the row
// beside its id — the drift baselines (a fingerprint, a version id, a
// timestamp) and the applied name or title. It is what the server keeps in a
// claim's `applied`, so a runner that lost its lock still has a drift baseline.
//
// The json tags are the WIRE format of `applied` and match the lock's own, so
// one row's anchor reads the same in both places. Adding a field here is safe
// for an older CLI: it merges the anchor it writes over the one it read, and
// keeps every key it does not know (see commands' mergeAnchor).
type folderAnchor struct {
	HeadVersionID string `json:"headVersionID,omitempty"`
	Title         string `json:"title,omitempty"`
}

// A table's CHECKS record and the DOCS-SIDECAR records are not here: both live
// on the FOLDER's anchor (FolderAnchor below), which depends on no claim. A
// sidecar documents a table this folder does not build, so it has no claim of
// its own; a checks record is one entry per check (~160 bytes), and on the
// table's claim — capped at 16 KiB by the server — a table with a hundred
// checks failed every anchor write.
type tableAnchor struct {
	LiveSHA256 string `json:"liveSHA256,omitempty"`
	MetaSHA256 string `json:"metaSHA256,omitempty"`
	Name       string `json:"name,omitempty"`
	Stem       string `json:"stem,omitempty"`
}

// LegacyChecksAnchorKey is the key an earlier build of this branch kept a
// table's checks record under on the table's OWN claim anchor. It is read once
// as a fallback, as LegacyDocsAnchorKey is (a folder anchor's record for the
// same checks file wins), and cleared from the claim once the folder anchor
// has been written (RetiredAnchorKeys).
const LegacyChecksAnchorKey = "checks"

type metricAnchor struct {
	LiveSHA256     string       `json:"liveSHA256,omitempty"`
	DeclaredSHA256 string       `json:"declaredSHA256,omitempty"`
	Name           string       `json:"name,omitempty"`
	Stem           string       `json:"stem,omitempty"`
	TagsApplied    []AppliedTag `json:"tagsApplied,omitempty"`
	TagsRefused    *TagsRefusal `json:"tagsRefused,omitempty"`
}

// LegacyDocsAnchorKey is the key an earlier build of this branch kept a
// folder's docs-sidecar records under on ONE claim's anchor (the "docs host":
// the first address the lock recorded). It lost them four ways — a docs-only
// folder had no host, an overflow erased them, a removed or refused host
// dropped them, a skipped host cleared the others — and the records now live
// on the folder anchor. A claim anchor that still carries the key is read ONCE
// as a fallback, while the folder anchor holds none (see TableDocsIn), and
// the key is cleared from it once the folder anchor has been written
// (RetiredAnchorKeys).
const LegacyDocsAnchorKey = "tableDocs"

type automationAnchor struct {
	UpdatedAt string `json:"updatedAt,omitempty"`
	Name      string `json:"name,omitempty"`
	Stem      string `json:"stem,omitempty"`
}

// AnchorKeys is every `applied` key this build reads and writes for a claim
// kind — the keys a merge may overwrite or clear. Any other key in a claim's
// anchor was written by a newer CLI and is carried through untouched.
func AnchorKeys(managedKind string) []string {
	var v any
	switch managedKind {
	case ManagedWorkflow, ManagedDataApp, ManagedModule:
		v = folderAnchor{}
	case ManagedTable:
		v = tableAnchor{}
	case ManagedMetric:
		v = metricAnchor{}
	case ManagedAutomation:
		v = automationAnchor{}
	default:
		return nil
	}
	keys := make([]string, 0, 6)
	for k := range jsonFieldNames(reflect.TypeOf(v)) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RetiredAnchorKeys are the claim-anchor keys this build no longer writes and
// clears once their content has a new home: the docs-sidecar records a table's
// or a metric's anchor carried before the folder anchor existed. A merge clears
// them only when told the new home holds the content (see commands'
// mergeAnchor), so a folder anchor that failed to record never costs the old
// copy.
func RetiredAnchorKeys(managedKind string) []string {
	switch managedKind {
	case ManagedTable:
		return []string{LegacyChecksAnchorKey, LegacyDocsAnchorKey}
	case ManagedMetric:
		return []string{LegacyDocsAnchorKey}
	}
	return nil
}

// Anchor is one address's anchor as this stack's lock records it, as the JSON
// object `applied` holds. Empty (never nil) when the lock records nothing there.
func (l *Lock) Anchor(stack string, kind Kind, address string) map[string]any {
	var v any = struct{}{}
	if l != nil {
		entry := l.Stacks[stack]
		switch ManagedKindAt(kind, address) {
		case ManagedWorkflow, ManagedDataApp, ManagedModule:
			v = folderAnchor{HeadVersionID: entry.HeadVersionID, Title: entry.Title}
		case ManagedTable:
			t := entry.Tables[address]
			v = tableAnchor{LiveSHA256: t.LiveSHA256, MetaSHA256: t.MetaSHA256, Name: t.Name, Stem: t.Stem}
		case ManagedMetric:
			m := entry.Metrics[address]
			v = metricAnchor{LiveSHA256: m.LiveSHA256, DeclaredSHA256: m.DeclaredSHA256, Name: m.Name, Stem: m.Stem,
				TagsApplied: m.TagsApplied, TagsRefused: m.TagsRefused}
		case ManagedAutomation:
			a := entry.Automations[address]
			v = automationAnchor{UpdatedAt: a.UpdatedAt, Name: a.Name, Stem: a.Stem}
		}
	}
	return toObject(v)
}

// THE FOLDER ANCHOR: the per-folder state a deploy records that belongs to no
// claim (backend rmanaged/folder.go, `managed_folders`), one per (stack's
// feature, folder id), written with a compare-and-set on its own revision.
//
// It holds the state that does not fit on a claim, and that is a decision
// rather than a default:
//
//   - the DOCS-SIDECAR records (LockStack.TableDocs), under "tableDocs" — the
//     only state a lock carries about the environment that is keyed by no row
//     this folder manages: a sidecar documents a table the folder does not
//     build;
//   - the health-CHECKS records (LockStack.Checks), under "checks", keyed by
//     checks file — one entry per check, which on its table's claim anchor
//     (16 KiB) failed every write for a table with a hundred checks.
//
// Everything else rides on its claim: a folder kind's head and title, a
// table's fingerprints, a metric's and an automation's anchors. Folder-level
// CONFIG (binds, dependencies, the address declarations) is not here either: it
// is in ronja.json, which every runner has.
//
// The server caps the anchor at 256 KiB and refuses, never truncates, past it.
type folderStateAnchor struct {
	TableDocs map[string]LockTableDocs `json:"tableDocs,omitempty"`
	Checks    map[string]LockChecks    `json:"checks,omitempty"`
}

// FolderAnchorKeys is every folder-anchor key this build reads and writes — the
// keys a merge may overwrite or clear. Any other key was written by a newer
// CLI and is carried through untouched.
func FolderAnchorKeys() []string {
	keys := make([]string, 0, 1)
	for k := range jsonFieldNames(reflect.TypeOf(folderStateAnchor{})) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FolderAnchor is this stack's folder-level state as the lock records it, as
// the JSON object a folder anchor's `applied` holds. Empty (never nil) when the
// lock records none.
func (l *Lock) FolderAnchor(stack string) map[string]any {
	if l == nil {
		return map[string]any{}
	}
	entry := l.Stacks[stack]
	return toObject(folderStateAnchor{TableDocs: entry.TableDocs, Checks: entry.Checks})
}

// SetFolderAnchor records a folder anchor's state in this stack's lock: the
// SERVER WINS for every sidecar path it names, and a path the lock records and
// the server does not is kept — a record lost is a drift guard disarmed, while
// one the server lacks is written to it by the next anchor write. Records are
// never pruned, here or anywhere (see SetTableDocsSeen).
func (l *Lock) SetFolderAnchor(stack string, applied map[string]any) error {
	if stack == "" || len(applied) == 0 {
		return nil
	}
	var a folderStateAnchor
	if err := fromObject(applied, &a); err != nil {
		return err
	}
	l.MergeTableDocs(stack, a.TableDocs)
	l.MergeChecks(stack, a.Checks)
	return nil
}

// MergeChecks writes checks records into this stack's lock, file by file — the
// given records win for every checks file they name; one the lock records and
// they do not is kept, as MergeTableDocs keeps a sidecar's.
func (l *Lock) MergeChecks(stack string, checks map[string]LockChecks) {
	if stack == "" || len(checks) == 0 {
		return
	}
	entry := l.stack(stack)
	if entry.Checks == nil {
		entry.Checks = map[string]LockChecks{}
	}
	for p, c := range checks {
		entry.Checks[p] = c
	}
	l.Stacks[stack] = entry
}

// TableDocsIn reads the docs-sidecar records an anchor object carries under
// "tableDocs" — a folder anchor's, or an older claim anchor's (see
// LegacyDocsAnchorKey) — or nil.
func TableDocsIn(applied map[string]any) (map[string]LockTableDocs, error) {
	raw, ok := applied[LegacyDocsAnchorKey]
	if !ok || raw == nil {
		return nil, nil
	}
	var a folderStateAnchor
	if err := fromObject(map[string]any{"tableDocs": raw}, &a); err != nil {
		return nil, err
	}
	return a.TableDocs, nil
}

// MergeTableDocs writes docs-sidecar records into this stack's lock, path by
// path — the given records win for every path they name.
func (l *Lock) MergeTableDocs(stack string, docs map[string]LockTableDocs) {
	if stack == "" || len(docs) == 0 {
		return
	}
	entry := l.stack(stack)
	if entry.TableDocs == nil {
		entry.TableDocs = map[string]LockTableDocs{}
	}
	for p, d := range docs {
		entry.TableDocs[p] = d
	}
	l.Stacks[stack] = entry
}

// SetManaged records the server's state for one address in this stack's lock:
// the row id, and — when anchor is non-nil — the anchor fields, REPLACED whole
// by what anchor says (a field it lacks is cleared). A nil anchor leaves the
// anchor fields as they are when the id is unchanged, and clears them when it
// is not: by the invariant every lock entry states, a baseline is only ever
// compared against the row it was taken from.
//
// Everything else the entry carries — the forward-compatibility sidecar, and
// for a table the checks and docs records keyed beside it — is left alone, but
// for a checks record an older build kept on the claim itself, read as a
// fallback (LegacyChecksAnchorKey).
func (l *Lock) SetManaged(stack string, kind Kind, address, id string, anchor map[string]any) error {
	if stack == "" {
		return fmt.Errorf("a managed folder's state is per stack, and this selection names none")
	}
	entry := l.stack(stack)
	switch ManagedKindAt(kind, address) {
	case ManagedWorkflow, ManagedDataApp, ManagedModule:
		current := entry.WorkflowID
		switch kind.Name {
		case KindDataApp:
			current = entry.DataAppID
		case KindModule:
			current = entry.ModuleID
		}
		if current != id {
			entry.HeadVersionID, entry.Title = "", ""
		}
		switch kind.Name {
		case KindWorkflow:
			entry.WorkflowID = id
		case KindDataApp:
			entry.DataAppID = id
		default:
			entry.ModuleID = id
		}
		if anchor != nil {
			var a folderAnchor
			if err := fromObject(anchor, &a); err != nil {
				return err
			}
			entry.HeadVersionID, entry.Title = a.HeadVersionID, a.Title
		}
	case ManagedTable:
		if entry.Tables == nil {
			entry.Tables = map[string]LockTable{}
		}
		t := entry.Tables[address]
		if t.TableID != id {
			t = LockTable{}
		}
		t.TableID = id
		if anchor != nil {
			var a tableAnchor
			if err := fromObject(anchor, &a); err != nil {
				return err
			}
			t.LiveSHA256, t.MetaSHA256, t.Name, t.Stem = a.LiveSHA256, a.MetaSHA256, a.Name, a.Stem
			// The checks record an older build kept on this claim
			// (LegacyChecksAnchorKey), read as a fallback: the folder anchor's
			// record for the same file, merged after the claims, wins.
			var legacy struct {
				Checks *LockChecks `json:"checks,omitempty"`
			}
			if err := fromObject(anchor, &legacy); err != nil {
				return err
			}
			if legacy.Checks != nil && legacy.Checks.TableID == id {
				if entry.Checks == nil {
					entry.Checks = map[string]LockChecks{}
				}
				entry.Checks[ChecksPath(fileStem(address))] = *legacy.Checks
			}
		}
		entry.Tables[address] = t
	case ManagedMetric:
		if entry.Metrics == nil {
			entry.Metrics = map[string]LockMetric{}
		}
		m := entry.Metrics[address]
		if m.MetricID != id {
			m = LockMetric{}
		}
		m.MetricID = id
		if anchor != nil {
			var a metricAnchor
			if err := fromObject(anchor, &a); err != nil {
				return err
			}
			m.LiveSHA256, m.DeclaredSHA256, m.Name, m.Stem = a.LiveSHA256, a.DeclaredSHA256, a.Name, a.Stem
			m.TagsApplied, m.TagsRefused = a.TagsApplied, a.TagsRefused
		}
		entry.Metrics[address] = m
	case ManagedAutomation:
		if entry.Automations == nil {
			entry.Automations = map[string]LockAutomation{}
		}
		a := entry.Automations[address]
		if a.AutomationID != id {
			a = LockAutomation{}
		}
		a.AutomationID = id
		if anchor != nil {
			var v automationAnchor
			if err := fromObject(anchor, &v); err != nil {
				return err
			}
			a.UpdatedAt, a.Name, a.Stem = v.UpdatedAt, v.Name, v.Stem
		}
		entry.Automations[address] = a
	default:
		return fmt.Errorf("%q is not an address a %s folder holds", address, kind.Label)
	}
	l.Stacks[stack] = entry
	return nil
}

// DropManaged forgets one address in this stack's lock — the lock losing track
// of a row, never the row itself. For a table, the checks record keyed by its
// stem goes with it, on DropPath's rule. A folder kind's single row is cleared
// with its anchor.
func (l *Lock) DropManaged(stack string, kind Kind, address string) {
	if l == nil {
		return
	}
	entry, ok := l.entry(stack)
	if !ok {
		return
	}
	switch kind.Name {
	case KindWorkflow:
		entry.WorkflowID, entry.HeadVersionID, entry.Title = "", "", ""
	case KindDataApp:
		entry.DataAppID, entry.HeadVersionID, entry.Title = "", "", ""
	case KindModule:
		entry.ModuleID, entry.HeadVersionID, entry.Title = "", "", ""
	default:
		l.Stacks[stack] = entry
		l.DropPath(stack, address)
		return
	}
	l.Stacks[stack] = entry
}

// DropRelocated forgets an address whose row the server's state holds at
// ANOTHER address of this folder (`to`) — a `moved` another run applied and
// whose lock was never committed. The server wins row → address exactly as it
// wins address → row: one row has one address, and an entry still naming it at
// the old one would make the declaration read as two rows at two addresses.
//
// For a table, the checks record keyed by the OLD stem goes too: its file was
// renamed with the table, and a record at a checks path no file holds is read as
// a deleted checks file, whose checks the next push would call orphans. It is
// carried to the new stem when nothing is recorded there yet (the claim's anchor
// had none), so the drift guard keeps its baseline; otherwise it is dropped.
func (l *Lock) DropRelocated(stack string, kind Kind, from, to string) {
	if l == nil {
		return
	}
	id := l.ManagedID(stack, kind, from)
	l.DropManaged(stack, kind, from)
	if ManagedKindAt(kind, from) != ManagedTable {
		return
	}
	oldChecks, newChecks := ChecksPath(fileStem(from)), ChecksPath(fileStem(to))
	if oldChecks == newChecks {
		return
	}
	entry, ok := l.entry(stack)
	if !ok {
		return
	}
	if rec, recorded := entry.Checks[oldChecks]; recorded && rec.TableID == id {
		if !rekey(entry.Checks, oldChecks, newChecks) {
			delete(entry.Checks, oldChecks)
		}
		l.Stacks[stack] = entry
	}
}

// fileStem is a folder-relative path's base name without its extension — the
// stem a table's checks file is named by.
func fileStem(p string) string {
	base := path.Base(p)
	return strings.TrimSuffix(base, path.Ext(base))
}

// toObject renders a value as a JSON object map — the shape `applied` has on
// the wire. A value that does not encode as an object answers an empty map.
func toObject(v any) map[string]any {
	out := map[string]any{}
	raw, err := json.Marshal(v)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// fromObject decodes an `applied` object into an anchor struct. Keys the struct
// has no field for are ignored here (they belong to a newer CLI, and the merge
// that writes an anchor back keeps them).
func fromObject(obj map[string]any, into any) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("read a claim's applied anchor: %w", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("read a claim's applied anchor: %w", err)
	}
	return nil
}
