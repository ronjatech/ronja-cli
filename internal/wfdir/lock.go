package wfdir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
)

// LockFormatVersion is the highest ronja.lock.json format this build can read.
//
// It carries the same machinery as the manifest's for the same reason, and the
// reason is worth stating rather than inferring from the symmetry: the lock file
// is COMMITTED and SHARED. Everything that makes an unknown key dangerous in
// ronja.json is true here — a colleague on an older CLI rewrites it, and
// whatever the newer format recorded is gone from the customer's repo — and the
// data at risk is worse, because the lock is what a CI checkout reads to know
// which rows it is updating rather than creating.
//
// 1, and an ABSENT version means 1, exactly as the manifest's does. Nothing
// WRITES the key at 1 — the file first appears with this change, so every lock
// file in existence is version 1 and says so by saying nothing — though a file
// that already declares 1 by hand decodes and is re-emitted as it was, since
// preserving what a committed file says is the whole habit of this package.
//
// ⚠️ When this number moves, MarshalJSON has to grow the stamp Manifest's has:
// raise to the version the CONTENT requires, never lower, and put the key first.
// There is nothing to stamp while the only version is 1, and writing the guard
// before there is anything to guard would be a rule with no case to test it.
const LockFormatVersion = 1

// Lock is ronja.lock.json — the committed, MACHINE-OWNED half of a folder.
//
// The split it completes: ronja.json says what a human decided (title,
// entrypoint, parameters, and which stack points where), and this file says what
// a deploy discovered (the ids it created, the fingerprints it agreed with).
// Before the split both lived in ronja.json, so every CI push dirtied the file a
// reviewer reads and every developer's pull rewrote it — and a fresh CI checkout
// had no baseline at all, which is why the loops documented --force as the way
// to run from CI.
//
// COMMITTED, and that is the point rather than an oversight. A lock file that
// stayed local would leave CI exactly where it was. It is also why it is not in
// .ronja/: that directory excludes itself with a "*" .gitignore, and this file
// has to be in the repo.
//
// Keyed by STACK NAME, so it can only describe a folder that has stacks. A
// legacy instances[]-only folder keeps its ids inline exactly as it always did
// and never grows a lock file — see SaveLock.
type Lock struct {
	// FormatVersion is the lock FORMAT, not the resource's. See
	// LockFormatVersion: omitempty, nothing writes it at 1, and a file that
	// declares 1 by hand keeps saying so.
	FormatVersion int `json:"formatVersion,omitempty"`
	// Stacks is the recorded state, one entry per stack name in ronja.json.
	//
	// A map keyed by name and NOT a list keyed by (url, tenantID): the lock's
	// whole job is to be the state half of a config entry, so it has to be keyed
	// by the same thing the config is. Keyed by url+tenant it would go stale the
	// moment a stack was repointed, and would answer for a stack that no longer
	// exists.
	Stacks map[string]LockStack `json:"stacks"`

	// unknown is the root object's forward-compatibility sidecar. See
	// unknownKeys in forwardcompat.go.
	unknown unknownKeys
}

// LockStack is what this folder's deploys to one stack created and last agreed
// with.
//
// Every field here answers "what did a deploy record", never "what did a person
// decide" — that is the test, and it is why featureID is NOT here: `wf init
// --feature <id>` is a human choosing where the workflow lives, so it sits in
// the stack's config in ronja.json even though the first push is what wrote it.
type LockStack struct {
	// WorkflowID and DataAppID are the STABLE identity of the row this folder's
	// pushes created, with the meaning Binding documents — never a per-user edit
	// draft's id, which would be a committed pointer at a row that stops existing
	// the moment its author publishes.
	WorkflowID string `json:"workflowID,omitempty"`
	DataAppID  string `json:"dataAppID,omitempty"`
	// ModuleID is a MODULE folder's created row, with the meaning the two above
	// carry. A module syncs its files into a per-user draft exactly as a
	// workflow does, so HeadVersionID below is its anchor too.
	ModuleID string `json:"moduleID,omitempty"`
	// HeadVersionID is the committed VERSION of the LIVE row that this folder's
	// content was forked from — the workflow/data-app answer to the question
	// LockTable.LiveSHA256 answers for a pipeline, and for exactly the same
	// reason: it is a fact about the ENVIRONMENT, true for everybody, so it is
	// the one thing a fresh checkout with no local baseline can be held to.
	//
	// A fingerprint could not do this job here. A workflow and a data app sync
	// their files into the CALLER'S OWN DRAFT, so every hash either kind has is
	// a hash of one person's row — committing one would hand a colleague a
	// baseline for a row they cannot see. The head version is the opposite: it
	// names a row everybody shares, and "has anybody published since we agreed"
	// is precisely the question a push must not answer by guessing.
	//
	// Empty means the guard is DISARMED and the folder behaves exactly as it did
	// before this field existed — a legacy instances[] folder (which has nowhere
	// committed to keep it), a stack last written by an older CLI, or a clone
	// that could not honestly claim an anchor. Empty is never treated as
	// agreement; it is treated as "no answer", which is the refusal.
	//
	// ⚠️ It describes WorkflowID/DataAppID above and nothing else. A stack
	// rebound to a different row drops it, by the same invariant LockTable
	// states: a pointer is only ever compared against the row it was read from.
	HeadVersionID string `json:"headVersionID,omitempty"`
	// Tables is a PIPELINE folder's per-file state: the slash-separated relative
	// path of each .sql file, the table it builds, and the fingerprint of that
	// LIVE row as of the last moment this folder agreed with it.
	//
	// The live fingerprint is here rather than in .ronja/state.json because of
	// the test that decides this whole file: it is a fact about the ENVIRONMENT,
	// not about a person. TableState's own doc already draws the line — the live
	// hash is "the LIVE row's SQL as of the last moment this folder agreed with
	// it", which is the same for everybody, while DraftID and DraftSHA256
	// describe one caller's open draft and stay local. Committed, it is the first
	// thing a fresh CI checkout has ever had to compare against, which is what
	// makes a CI push safe without --force.
	Tables map[string]LockTable `json:"tables,omitempty"`
	// Automations is an AUTOMATION folder's per-file state: the id of the
	// scheduled_jobs row each .json file describes, and when that row last
	// changed as the server reported it.
	//
	// Keyed by the slash-separated relative path exactly as Enumerate reports it
	// and exactly as Tables is, so both committed files agree about what a file
	// is called on every platform.
	//
	// Committed for Tables' reason: it is a fact about the ENVIRONMENT — which
	// row this path is, and when everybody last saw it move — not about a person.
	// Here it is the ONLY such fact there is: an automation has no draft, so
	// there is no per-user half to leave in .ronja/state.json.
	//
	// ⚠️ Losing an entry from this map is not a degraded lookup. LockAutomation
	// below states what it is instead, and it is the one thing to read before
	// touching any writer of this map.
	Automations map[string]LockAutomation `json:"automations,omitempty"`
	// TableDocs is a PIPELINE folder's per-DOCS-SIDECAR state: the table each
	// committed docs file describes, and that row's documentation fingerprint as
	// of the last moment this folder agreed with it.
	//
	// Keyed by the sidecar's slash-separated relative path (`tables/orders.json`),
	// exactly as Tables and Automations are keyed by theirs.
	//
	// SEPARATE FROM Tables, and the separation is the design rather than an
	// accident of typing. Tables is keyed by a .sql file — a table this folder
	// BUILDS — and a sidecar exists precisely for the tables it does not: an
	// integration table, a foundation table, one a workflow writes. Those have no
	// .sql file to key on and never will, so folding the two maps together would
	// mean inventing a fake path for a file that does not exist.
	//
	// Committed for Tables' reason: which row a path documents, and what that
	// row's prose looked like when everybody last agreed with it, are facts about
	// the ENVIRONMENT rather than about a person. A sidecar writes straight to
	// the live row — there is no draft in this path — so unlike a pipeline file
	// there is no per-user half at all.
	TableDocs map[string]LockTableDocs `json:"tableDocs,omitempty"`
	// Metrics is a PIPELINE folder's per-METRIC-FILE state: the metric each
	// committed `metrics/<alias>.json` defines, that row's recipe fingerprint as
	// of the last moment this folder agreed with it, and the fingerprint of what
	// the file itself last declared.
	//
	// Keyed by the metric file's slash-separated relative path
	// (`metrics/average_order_value.json`), exactly as Tables, Automations and
	// TableDocs are keyed by theirs.
	//
	// SEPARATE FROM Tables for TableDocs' reason and one more of its own. Tables
	// is keyed by a .sql file, and a metric has none — a metric is a recipe, so
	// there is no SQL a file could hold and no path in that map to key on. The
	// two also carry different fingerprints of different things: LockTable's
	// LiveSHA256 is the live table's SQL, this one is the live metric's
	// canonicalized RECIPE, and folding them would compare a recipe against a
	// hash taken from somebody's SELECT.
	//
	// Committed for Tables' reason: which row a path defines, and what that row's
	// definition looked like when everybody last agreed with it, are facts about
	// the ENVIRONMENT rather than about a person — and here that matters more
	// than anywhere else in this file, because the row in question is the
	// company's official definition of a number.
	Metrics map[string]LockMetric `json:"metrics,omitempty"`

	// ⚠️ There is deliberately NO per-file fingerprint for a workflow or a data
	// app here, and the asymmetry with Tables above is not an oversight. Those
	// two kinds sync their files into the caller's own DRAFT, so
	// InstanceState.Files describes one person's row: committing it would hand a
	// colleague a baseline for a row they cannot see, which is the reason
	// .ronja/ is git-ignored in the first place. A pipeline's live hash is a
	// different number about a different row — the LIVE table, the same for
	// everybody — which is why it is the one that could move.
	//
	// HeadVersionID above is what those two kinds have instead, and it is a
	// pointer rather than a fingerprint for that same reason.

	// unknown is this stack entry's forward-compatibility sidecar.
	unknown unknownKeys
}

// LockTable is one pipeline file's recorded state on one stack.
//
// ⚠️ LiveSHA256 obeys the invariant on wfdir.TableState and it is the whole
// reason these hashes are separate values: A HASH IS ONLY EVER COMPARED AGAINST
// THE ROW IT WAS TAKEN FROM. This one is taken from the LIVE table and compared
// against the live table. The draft's fingerprint is a different number about a
// different row, it is per-user, and it stays in .ronja/state.json.
type LockTable struct {
	TableID    string `json:"tableID"`
	LiveSHA256 string `json:"liveSHA256,omitempty"`
	// MetaSHA256 is the LIVE row's DOCUMENTATION as of the last moment this
	// folder agreed with it — its description plus every column that carries
	// prose, hashed by tabledocs.MetaSHA256.
	//
	// A third leg beside LiveSHA256, and a separate value for the invariant this
	// type states rather than for tidiness: it is taken from a different part of
	// the row and answers a different question, so folding it into the SQL hash
	// would report "the SQL changed" for a description somebody edited in the web
	// app — and, worse, would make every existing folder's recorded LiveSHA256
	// wrong on the first push after this field shipped.
	//
	// It ARMS ONLY for a push that carries documentation. A folder whose .sql
	// files open with no `-- @table` header declares nothing about a table's
	// prose, so there is nothing for the guard to protect and nothing is
	// recorded — which is what keeps every folder written before this feature
	// behaving exactly as it did.
	//
	// Empty means the guard is DISARMED — "no answer", never "agreed" — exactly
	// as an absent LiveSHA256 disarms leg (a).
	MetaSHA256 string `json:"metaSHA256,omitempty"`

	// unknown is this table entry's forward-compatibility sidecar, and it is
	// here for the same reason Lock and LockStack have one: it is an object in a
	// COMMITTED file, so a key a newer CLI writes into it has to survive an
	// older one rewriting the file around it. The sidecars stopped one nesting
	// level short of this until the level itself was new — a gap worth naming,
	// because the next thing that goes in here is a per-table version pointer,
	// and losing THAT to a colleague's older CLI is a folder that no longer
	// knows which version of the table it agreed with.
	//
	// Not a case for widening BL-8c4f, which is about unknown keys inside the
	// api request types (WorkflowParameter, DataAppAccess) — those are the wire
	// format too, so preserving them is a decision about what the CLI SENDS.
	// This type is written to disk and nowhere else.
	unknown unknownKeys
}

// LockTableDocs is one docs sidecar's recorded state on one stack.
//
// ⚠️ MetaSHA256 obeys the invariant on LockTable, and it is the same invariant
// for the same reason: A HASH IS ONLY EVER COMPARED AGAINST THE ROW IT WAS TAKEN
// FROM. TableID is which row that was, so a sidecar rebound to a different table
// — the alias repointed by `ronja bind`, a stack switched, a merge — drops the
// fingerprint with it rather than comparing one table's prose against a hash
// taken from another's.
type LockTableDocs struct {
	// TableID is the row this sidecar's alias resolved to when the fingerprint
	// was taken. It duplicates what the manifest's bind map says today on
	// purpose: the recording has to name the row it describes, not merely that
	// it describes one.
	TableID string `json:"tableID"`
	// MetaSHA256 is that row's documentation fingerprint (tabledocs.MetaSHA256).
	// Empty DISARMS the guard — "no answer", never "agreed" — which is the state
	// of a folder cloned from git and of one written before this field existed.
	MetaSHA256 string `json:"metaSHA256,omitempty"`
	// DeclaredSHA256 is the fingerprint of what the FILE declared when it was
	// last pushed (tabledocs.Docs.Fingerprint) — this sidecar's answer to "has
	// the local file changed since the last sync", which for a .sql file is
	// answered by the folder's own content baseline.
	//
	// It needs its own field because a sidecar is not in that baseline at all: a
	// pipeline folder syncs `.sql`, so the enumeration never sees these files.
	//
	// ⚠️ TWO FINGERPRINTS OF TWO DIFFERENT THINGS, and the invariant this type
	// states is why they are not one value: MetaSHA256 is taken from the ROW and
	// compared against the row, this one is taken from the FILE and compared
	// against the file. Comparing the file against the row cannot work — a
	// documented name the table does not have is stored server-side and never
	// reported back, so such a sidecar would read as pending for ever and be
	// re-sent on every push.
	DeclaredSHA256 string `json:"declaredSHA256,omitempty"`

	// unknown is this entry's forward-compatibility sidecar, for the reason
	// LockTable's has one: it is an object in a COMMITTED file.
	unknown unknownKeys
}

// LockMetric is one metric file's recorded state on one stack.
//
// ⚠️ LiveSHA256 obeys the invariant on LockTable, and it is the same invariant
// for the same reason: A HASH IS ONLY EVER COMPARED AGAINST THE ROW IT WAS TAKEN
// FROM. MetricID is which row that was, so a metric file rebound to a different
// row — the alias repointed by `ronja bind`, a stack switched, a merge — drops
// the fingerprint with it rather than comparing one metric's definition against a
// hash taken from another's.
type LockMetric struct {
	// MetricID is the row this file's stem resolved to when the fingerprint was
	// taken. It duplicates what the manifest's bind map says today on purpose:
	// the recording has to name the row it describes, not merely that it
	// describes one.
	MetricID string `json:"metricID"`
	// LiveSHA256 is the LIVE metric's recipe fingerprint (metricfile.RecipeSHA256
	// over the bytes the server sent). Empty DISARMS the guard — "no answer",
	// never "agreed" — which is the state of a folder cloned from git and of one
	// written before this field existed.
	//
	// ⚠️ IT IS TAKEN OVER THE ROW'S CANONICALIZED RECIPE, NOT OVER THE FILE. The
	// file carries an alias in `source` and may omit a defaultable key; the row
	// carries a real id and every default filled in. Hashing the file here would
	// report drift on every stack, on the first push, for ever.
	LiveSHA256 string `json:"liveSHA256,omitempty"`
	// DeclaredSHA256 is the fingerprint of what the FILE declared when it was
	// last pushed (metricfile.Fingerprint over the resolved recipe and both
	// three-state fields) — this metric's answer to "has the local file changed
	// since the last sync", which for a .sql file is answered by the folder's own
	// content baseline.
	//
	// It needs its own field for LockTableDocs.DeclaredSHA256's two reasons. A
	// metric file is not in that baseline at all — a pipeline folder syncs
	// `.sql`, so the enumeration never sees these files — and the file cannot be
	// compared against the row directly, because the row holds the canonicalized
	// recipe and the file does not. Without it every push would re-write, re-sync
	// and re-BUILD every metric in the folder, which is expensive and, on a
	// metric whose commit re-fingerprints, noisy in governance too.
	DeclaredSHA256 string `json:"declaredSHA256,omitempty"`

	// unknown is this entry's forward-compatibility sidecar, for the reason
	// LockTable's has one: it is an object in a COMMITTED file.
	unknown unknownKeys
}

// LockAutomation is one automation file's recorded state on one stack.
//
// Deliberately NO content fingerprint, and the asymmetry with LockTable is the
// point. A pipeline commits LiveSHA256 because the live table's SQL is the only
// thing a fresh checkout could otherwise compare against, and workflows and data
// apps commit HeadVersionID instead because their files live in a per-user
// draft. An automation needs neither: since GET :jobID returns the action, the
// id in AutomationID plus one read gives the whole live config back, so a
// committed hash would be a second answer to a question the server already
// answers exactly — and a second answer is a thing that can go stale.
type LockAutomation struct {
	// AutomationID is the scheduled_jobs row this file describes.
	//
	// REQUIRED, and it is the whole reason this map exists. scheduled_jobs.name
	// is neither unique nor required, so there is no second way to find the row:
	// an entry that goes missing is not a lookup that falls back to a search, it
	// is a push that CREATES a duplicate automation beside the live one, which
	// then fires alongside it.
	AutomationID string `json:"automationID"`
	// UpdatedAt is the row's `updatedAt` as the server last reported it,
	// rendered by api.Automation.Stamp — one spelling, in one place, because
	// this value is compared against ITSELF across runs and two writers
	// formatting it differently would report drift on a row nobody touched.
	//
	// ⚠️ IT IS NOT A CONTENT FINGERPRINT, and reading it as one is how it gets
	// dropped as redundant. It is the only ANCHOR this loop has: unlike a
	// workflow or a data app there is no HeadVersionID here, and unlike a
	// pipeline there is no draft/publish CAS — scheduled_jobs has no version
	// history at all, so two writers are otherwise last-write-wins with PUT's
	// list fields REPLACING rather than merging.
	//
	// Two refusals are built on it, and both are silent failures without it:
	//
	//   - A push whose row has moved since this timestamp is refused and points
	//     at `status`, rather than overwriting whatever somebody changed in the
	//     web app between the read and the write.
	//   - A push flipping `enabled` false→true is refused when the row was
	//     disabled AFTER this timestamp. A human pause (disabled_reason='user')
	//     is clearable, so without this gate a folder declaring enabled:true — a
	//     line added months ago to stop `status` reporting drift — silently
	//     reverses an incident response on the next CI merge.
	//
	// Empty DISARMS both, exactly as an absent LiveSHA256 disarms a pipeline's
	// drift guard: "no answer", never "agreed".
	UpdatedAt string `json:"updatedAt,omitempty"`

	// unknown is this entry's forward-compatibility sidecar, for the reason
	// LockTable's has one: it is an object in a COMMITTED file, so a key a newer
	// CLI writes into it has to survive an older one rewriting the file around it.
	unknown unknownKeys
}

// lockKeys / lockStackKeys are the object keys each type marshals to, so the
// sidecars can tell "a key this CLI owns" from "a key from the future".
var (
	lockKeys           = jsonFieldNames(reflect.TypeOf(Lock{}))
	lockStackKeys      = jsonFieldNames(reflect.TypeOf(LockStack{}))
	lockTableKeys      = jsonFieldNames(reflect.TypeOf(LockTable{}))
	lockTableDocsKeys  = jsonFieldNames(reflect.TypeOf(LockTableDocs{}))
	lockMetricKeys     = jsonFieldNames(reflect.TypeOf(LockMetric{}))
	lockAutomationKeys = jsonFieldNames(reflect.TypeOf(LockAutomation{}))
	stackKeys          = jsonFieldNames(reflect.TypeOf(Stack{}))
)

// UnmarshalJSON / MarshalJSON keep the keys this build has no field for, for the
// reasons on Manifest's pair. plainLock is the method-less twin that stops the
// recursion; see forwardcompat.go.
func (l *Lock) UnmarshalJSON(data []byte) error {
	var decoded plainLock
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*l = Lock(decoded)
	l.unknown.capture(data, lockKeys)
	return nil
}

// MarshalJSON writes the struct's own keys and puts the unrecognised ones back.
//
// It does NOT stamp a format version, because there is only one and an absent
// key already means it — see the ⚠️ on LockFormatVersion for what has to happen
// here the day that changes.
//
// A VALUE receiver, so the empty-map substitution below lands on this copy and
// not on the caller's struct, and so a LockStack inside a map (which is never
// addressable) is marshalled through its own method too.
func (l Lock) MarshalJSON() ([]byte, error) {
	if l.Stacks == nil {
		// An empty OBJECT rather than null. The file is written only when it has
		// content or already exists, so a null here would only ever be the
		// tombstone left by removing the last stack — and `"stacks": null` reads
		// like a corruption to whoever opens it next.
		l.Stacks = map[string]LockStack{}
	}
	body, err := json.Marshal(plainLock(l))
	if err != nil {
		return nil, err
	}
	return l.unknown.merge(body)
}

func (s *LockStack) UnmarshalJSON(data []byte) error {
	var decoded plainLockStack
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = LockStack(decoded)
	s.unknown.capture(data, lockStackKeys)
	return nil
}

func (s LockStack) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(plainLockStack(s))
	if err != nil {
		return nil, err
	}
	return s.unknown.merge(body)
}

func (t *LockTable) UnmarshalJSON(data []byte) error {
	var decoded plainLockTable
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*t = LockTable(decoded)
	t.unknown.capture(data, lockTableKeys)
	return nil
}

func (t LockTable) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(plainLockTable(t))
	if err != nil {
		return nil, err
	}
	return t.unknown.merge(body)
}

func (d *LockTableDocs) UnmarshalJSON(data []byte) error {
	var decoded plainLockTableDocs
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*d = LockTableDocs(decoded)
	d.unknown.capture(data, lockTableDocsKeys)
	return nil
}

func (d LockTableDocs) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(plainLockTableDocs(d))
	if err != nil {
		return nil, err
	}
	return d.unknown.merge(body)
}

func (m *LockMetric) UnmarshalJSON(data []byte) error {
	var decoded plainLockMetric
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = LockMetric(decoded)
	m.unknown.capture(data, lockMetricKeys)
	return nil
}

func (m LockMetric) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(plainLockMetric(m))
	if err != nil {
		return nil, err
	}
	return m.unknown.merge(body)
}

func (a *LockAutomation) UnmarshalJSON(data []byte) error {
	var decoded plainLockAutomation
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*a = LockAutomation(decoded)
	a.unknown.capture(data, lockAutomationKeys)
	return nil
}

func (a LockAutomation) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(plainLockAutomation(a))
	if err != nil {
		return nil, err
	}
	return a.unknown.merge(body)
}

func (s *Stack) UnmarshalJSON(data []byte) error {
	var decoded plainStack
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = Stack(decoded)
	s.unknown.capture(data, stackKeys)
	return nil
}

func (s Stack) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(plainStack(s))
	if err != nil {
		return nil, err
	}
	return s.unknown.merge(body)
}

// LockPath locates the lock file inside a folder root.
func LockPath(root string) string { return filepath.Join(root, LockName) }

// LoadLock reads the lock file. A missing one is NOT an error: it is what a
// legacy instances[] folder has, and what a stack folder has before its first
// push. Callers get an empty Lock and read nothing from it.
func LoadLock(root string) (*Lock, error) {
	path := LockPath(root)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Lock{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// The format gate runs BEFORE the decode, off a one-key probe, for the
	// reason it does on the manifest: a file from the future is exactly the one
	// whose decode cannot be trusted to fail legibly.
	if err := checkFormatVersion(path, raw, LockFormatVersion); err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(raw, &l); err != nil {
		rewriteInternalTypeNames(err)
		// The remedy names re-pushing rather than hand-editing: unlike the
		// manifest, nothing in here is a human's decision, so the honest fix for a
		// corrupt lock is to let the next push rewrite it.
		return nil, fmt.Errorf("parse %s: %w (it is machine-written state — deleting it and pushing again rebuilds it, at the cost of the drift baseline it held)", path, err)
	}
	return &l, nil
}

// SaveLock writes the lock file, indented and newline-terminated because it is
// committed and diffed like the manifest.
//
// It writes NOTHING for a folder that has no stacks and no lock file already —
// which is precisely a legacy instances[] folder. That is the mechanism behind
// "a v1 folder keeps round-tripping as v1": push, discard and publish all call
// this, and on a folder nobody has named a stack in, all of them leave the
// directory exactly as they found it.
func SaveLock(root string, l *Lock) error {
	path := LockPath(root)
	if len(l.Stacks) == 0 {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	}
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("encode lock file: %w", err)
	}
	return writeAtomic(path, append(body, '\n'), 0o644)
}

// SaveFolder writes both committed files, and is what every caller should use
// rather than SaveManifest alone: a manifest that names a stack whose lock entry
// never landed is a folder whose next push reads it as unbound and creates a
// SECOND set of resources.
//
// The LOCK goes first, deliberately. The two orders fail differently and only
// one of them fails safely, and it is worth being exact about why — the
// original claim here, that a lock entry with no stack naming it is "inert
// because no selection can reach it", was the wrong reason for the right
// ordering. Unreachable would have been the WORSE property, not the safer one:
// a created workflow id recorded in the lock and unreachable is a resource that
// exists and that the next push creates AGAIN. What actually makes this order
// safe is that selectNamed treats a lock entry under the requested name as
// BOUND, so `--stack <name>` picks the stranded ids up rather than duplicating
// them. The other order — manifest first — has no such recovery: a stack with
// no lock entry reads as never-pushed, which is the duplicate-creation above.
// Neither file has a transaction to offer, so the choice is which half to leave
// behind and which half can be recovered from.
func SaveFolder(root string, m *Manifest, l *Lock) error {
	if err := SaveLock(root, l); err != nil {
		return err
	}
	return SaveManifest(root, m)
}

// Binding is the recorded state for one stack, in the shape every command
// already reads. FeatureID is deliberately NOT filled in here — it is config and
// lives in the manifest's stack — so callers go through Manifest.Select rather
// than reading the lock directly.
//
// A nil receiver answers the zero value, which is what "no lock file" means:
// nothing has been created here yet.
func (l *Lock) Binding(stack string) Binding {
	if l == nil {
		return Binding{}
	}
	entry, ok := l.Stacks[stack]
	if !ok {
		return Binding{}
	}
	b := Binding{WorkflowID: entry.WorkflowID, DataAppID: entry.DataAppID, ModuleID: entry.ModuleID}
	if len(entry.Tables) > 0 {
		b.Tables = make(map[string]string, len(entry.Tables))
		for path, table := range entry.Tables {
			b.Tables[path] = table.TableID
		}
	}
	if len(entry.Automations) > 0 {
		b.Automations = make(map[string]string, len(entry.Automations))
		for path, automation := range entry.Automations {
			b.Automations[path] = automation.AutomationID
		}
	}
	return b
}

// SetBinding records one stack's created ids, preserving the per-table live
// fingerprints the binding does not carry.
//
// Preserving them is not tidiness: Binding is path → table id, so writing the
// map wholesale would drop every LiveSHA256 on the first push after a clone, and
// the drift guard would silently stop having anything to compare against — the
// exact failure the lock exists to fix, reintroduced by the writer.
//
// An automation folder's Automations map is preserved the same way and for the
// sharper version of that reason: the value it would drop is UpdatedAt, and
// losing that does not degrade a guard — it disarms the two that
// LockAutomation.UpdatedAt names.
func (l *Lock) SetBinding(stack string, b Binding) {
	_, existed := l.entry(stack)
	if !existed && b.WorkflowID == "" && b.DataAppID == "" && b.ModuleID == "" && len(b.Tables) == 0 && len(b.Automations) == 0 {
		// Nothing to record and nothing recorded before. `wf init --stack dev`
		// declares the stack and creates nothing, and writing `{"stacks":{"dev":
		// {}}}` for it puts a committed file into the customer's repo that says
		// nothing at all — and, worse, would be the first thing a reader of the
		// lock file ever sees. The first push writes it with an id in it.
		//
		// Only when the entry is NEW. An existing one emptied by `discard
		// --delete-workflow` stays, because "this stack has been pushed to and
		// has nothing now" is a real thing to have recorded.
		return
	}
	entry := l.stack(stack)
	// A stack REPOINTED at a different row drops the head pointer with it, by
	// the invariant on HeadVersionID — the same one SetTableLive enforces for a
	// rebound path. Kept, it would be a version id read from one workflow and
	// compared against another's history, where it can only ever fail to match:
	// a guard that refuses every push and names a version nobody recognises.
	if entry.WorkflowID != b.WorkflowID || entry.DataAppID != b.DataAppID || entry.ModuleID != b.ModuleID {
		entry.HeadVersionID = ""
	}
	entry.WorkflowID = b.WorkflowID
	entry.DataAppID = b.DataAppID
	entry.ModuleID = b.ModuleID
	if b.Tables == nil {
		entry.Tables = nil
	} else {
		tables := make(map[string]LockTable, len(b.Tables))
		for path, id := range b.Tables {
			existing := entry.Tables[path]
			// A path rebound to a DIFFERENT table drops the old row's fingerprint
			// with it. Keeping it would compare a colleague's table against a hash
			// taken from something else entirely and conclude nothing had changed —
			// the invariant on LockTable, enforced at the one place a rebinding can
			// happen.
			if existing.TableID != id {
				existing = LockTable{}
			}
			existing.TableID = id
			tables[path] = existing
		}
		entry.Tables = tables
	}
	if b.Automations == nil {
		entry.Automations = nil
	} else {
		automations := make(map[string]LockAutomation, len(b.Automations))
		for path, id := range b.Automations {
			existing := entry.Automations[path]
			// A path rebound to a DIFFERENT automation drops the old row's
			// timestamp with it, by the invariant LockTable states for its hash:
			// an anchor is only ever compared against the row it was read from.
			// Kept, it would be one row's updatedAt tested against another's —
			// which either refuses every push (naming a change nobody made) or,
			// if the other row happens to be older, waves through the very
			// overwrite the anchor exists to catch.
			if existing.AutomationID != id {
				existing = LockAutomation{}
			}
			existing.AutomationID = id
			automations[path] = existing
		}
		entry.Automations = automations
	}
	l.Stacks[stack] = entry
}

// entry reports whether the lock records anything at all under a stack name,
// and what.
//
// Distinct from Binding, which answers the zero value for a name it does not
// have: "recorded nothing" and "not recorded" are the same Binding and very
// different answers to selectNamed, where one is a stranded created id to adopt
// and the other is a stack nobody has pushed.
//
// A nil receiver, or no lock file, reports not recorded.
func (l *Lock) entry(stack string) (LockStack, bool) {
	if l == nil {
		return LockStack{}, false
	}
	entry, ok := l.Stacks[stack]
	return entry, ok
}

// TableLive reads one pipeline file's recorded live fingerprint, empty when
// there is none — which disarms that leg of the drift guard exactly as an absent
// local baseline does.
func (l *Lock) TableLive(stack, path string) string {
	if l == nil {
		return ""
	}
	return l.Stacks[stack].Tables[path].LiveSHA256
}

// HeadVersion reads the committed head-version pointer for one stack, empty
// when there is none — which disarms that leg of the drift guard exactly as an
// absent live fingerprint does for a pipeline, and as an absent local baseline
// does for everything.
func (l *Lock) HeadVersion(stack string) string {
	if l == nil {
		return ""
	}
	return l.Stacks[stack].HeadVersionID
}

// SetHeadVersion records the live version this folder now agrees with.
//
// Called only at the moments that is TRUE, which is the whole discipline of the
// field: a clone that took the live row's files, a first push that created the
// row (nothing has been published, so the row is its own head), a push onto a
// draft forked from the recorded version, and a publish that just committed a
// new version.
//
// Never called with a head read while somebody else's version sat between it and
// this folder's content — see workflow_clone's draft branch, and
// dataAppCloneAnchor's versioned-draft leg, both of which record nothing rather
// than over-claim. A --force push past a version this folder never saw CLEARS
// the pointer for the same reason: it must stop re-reporting the difference
// without pretending to have seen the version on the other side of it.
//
// An empty id CLEARS the pointer, which is how a caller says "I cannot honestly
// anchor this" without having to know whether one was there before.
func (l *Lock) SetHeadVersion(stack, versionID string) {
	entry := l.stack(stack)
	if entry.HeadVersionID == versionID {
		return
	}
	entry.HeadVersionID = versionID
	l.Stacks[stack] = entry
}

// SetTableLive records the LIVE row's fingerprint as the one this folder agrees
// with. Called at the three moments that is true — a clone, the fork of a fresh
// draft, and a publish that just committed — and never with a draft's SQL, for
// the reason recordLiveAgreement documents.
func (l *Lock) SetTableLive(stack, path, tableID, sha string) {
	entry := l.stack(stack)
	if entry.Tables == nil {
		entry.Tables = map[string]LockTable{}
	}
	// Read-edit-store rather than a fresh value, so the forward-compatibility
	// sidecar survives — a rewritten value built from scratch is exactly how the
	// keys a newer CLI wrote get dropped. Only for the SAME table: a path
	// rebound to a different row keeps nothing, by the invariant on LockTable.
	table := entry.Tables[path]
	if table.TableID != tableID {
		table = LockTable{}
	}
	table.TableID, table.LiveSHA256 = tableID, sha
	entry.Tables[path] = table
	l.Stacks[stack] = entry
}

// TableMeta reads the DOCUMENTATION fingerprint this folder last agreed with for
// one pipeline file's live table. Empty for a path with no recording, which is
// what disarms the third drift leg — "no answer", never "agreed".
func (l *Lock) TableMeta(stack, path string) string {
	if l == nil {
		return ""
	}
	return l.Stacks[stack].Tables[path].MetaSHA256
}

// SetTableMeta records the LIVE row's documentation fingerprint as the one this
// folder agrees with.
//
// Called only where that is TRUE — a live row this command just READ, or one it
// just committed onto — and never with a fingerprint taken from a draft. A
// draft's prose is not the live row's, and recording it here would report
// agreement with a state nobody else can see, then refuse every later push as
// drift on a table that never moved.
//
// An empty sha CLEARS the recording, which is how a caller says "I can no longer
// honestly claim to have seen this row's documentation" — a publish that could
// not re-read the row it just wrote — without having to know whether one was
// there before.
func (l *Lock) SetTableMeta(stack, path, tableID, sha string) {
	entry := l.stack(stack)
	if entry.Tables == nil {
		entry.Tables = map[string]LockTable{}
	}
	// Read-edit-store, so the forward-compatibility sidecar survives; and only
	// for the SAME table, by the invariant on LockTable.
	table := entry.Tables[path]
	if table.TableID != tableID {
		table = LockTable{}
	}
	table.TableID, table.MetaSHA256 = tableID, sha
	entry.Tables[path] = table
	l.Stacks[stack] = entry
}

// TableDocsSeen reads one docs sidecar's recorded table id and the documentation
// fingerprint this folder last agreed with. An unrecorded path answers two empty
// strings, which disarms the guard.
func (l *Lock) TableDocsSeen(stack, path string) (tableID, meta, declared string) {
	if l == nil {
		return "", "", ""
	}
	entry := l.Stacks[stack].TableDocs[path]
	return entry.TableID, entry.MetaSHA256, entry.DeclaredSHA256
}

// SetTableDocsSeen records the row a docs sidecar describes and that row's
// documentation fingerprint at the moment this folder agreed with it.
//
// ⚠️ A LEGACY instances[] folder has no stack name, and there is nowhere in a
// lock keyed by stack name to put its recording — writing one anyway puts
// `"stacks":{"":{…}}` into a committed file, which is the bug SetAutomationSeen
// names. Its recording lives in .ronja/state.json instead; see the liveHashes
// fork in the commands package.
func (l *Lock) SetTableDocsSeen(stack, path, tableID, meta, declared string) {
	if stack == "" {
		return
	}
	entry := l.stack(stack)
	if entry.TableDocs == nil {
		entry.TableDocs = map[string]LockTableDocs{}
	}
	docs := entry.TableDocs[path]
	if docs.TableID != tableID {
		docs = LockTableDocs{}
	}
	docs.TableID, docs.MetaSHA256, docs.DeclaredSHA256 = tableID, meta, declared
	entry.TableDocs[path] = docs
	l.Stacks[stack] = entry
}

// MetricSeen reads one metric file's recorded row id, the live metric's recipe
// fingerprint this folder last agreed with, and what the file itself last
// declared. An unrecorded path answers three empty strings, which disarms both
// guards.
func (l *Lock) MetricSeen(stack, path string) (metricID, live, declared string) {
	if l == nil {
		return "", "", ""
	}
	entry := l.Stacks[stack].Metrics[path]
	return entry.MetricID, entry.LiveSHA256, entry.DeclaredSHA256
}

// SetMetricSeen records the row a metric file defines, that row's recipe
// fingerprint at the moment this folder agreed with it, and what the file
// declared then.
//
// ⚠️ A LEGACY instances[] folder has no stack name, and there is nowhere in a
// lock keyed by stack name to put its recording — writing one anyway puts
// `"stacks":{"":{…}}` into a committed file, which is the bug SetAutomationSeen
// names. Its recording lives in .ronja/state.json instead; see the liveHashes
// fork in the commands package.
func (l *Lock) SetMetricSeen(stack, path, metricID, live, declared string) {
	if stack == "" {
		return
	}
	entry := l.stack(stack)
	if entry.Metrics == nil {
		entry.Metrics = map[string]LockMetric{}
	}
	// Read-edit-store rather than a fresh value, so the forward-compatibility
	// sidecar survives — a rewritten value built from scratch is exactly how the
	// keys a newer CLI wrote get dropped. Only for the SAME row: a path rebound
	// to a different metric keeps nothing, by the invariant on LockMetric.
	metric := entry.Metrics[path]
	if metric.MetricID != metricID {
		metric = LockMetric{}
	}
	metric.MetricID, metric.LiveSHA256, metric.DeclaredSHA256 = metricID, live, declared
	entry.Metrics[path] = metric
	l.Stacks[stack] = entry
}

// AutomationSeen reads one automation file's recorded row id and the `updatedAt`
// this folder last agreed with. An unrecorded path answers two empty strings,
// which is what disarms the drift and re-enable guards — "no answer", never
// "agreed".
func (l *Lock) AutomationSeen(stack, path string) (automationID, updatedAt string) {
	if l == nil {
		return "", ""
	}
	entry := l.Stacks[stack].Automations[path]
	return entry.AutomationID, entry.UpdatedAt
}

// SetAutomationSeen records the row this path is, and the `updatedAt` the server
// reported for it at the moment this folder agreed with it.
//
// Called only where that is TRUE — the read a status or a push just made, and
// the response of a create or an update — and never with a timestamp from
// anywhere else. An updatedAt taken from one response and stored against a later
// state of the row is worse than none: it reports agreement with a version
// nobody read.
//
// An empty updatedAt CLEARS the anchor, which is how a caller says "I recorded
// the id but cannot honestly claim to have seen the row's current state" without
// having to know whether one was there before.
func (l *Lock) SetAutomationSeen(stack, path, automationID, updatedAt string) {
	if stack == "" {
		// ⚠️ A LEGACY instances[] folder has no stack name, and its bindings live
		// in the manifest — there is nowhere in a lock keyed by stack name to put
		// its anchor. Writing one anyway put `"stacks":{"":{…}}` into a committed
		// file, and since nothing ever refreshed it, every later push refused
		// "this changed on the server" for ever, --force included.
		//
		// Guarded HERE rather than at each call site, which is what SetBinding's
		// own refusal to write an empty entry does: one guard at the write cannot
		// be forgotten by the next caller, and the two call sites in
		// automation_push.go had already disagreed about it.
		return
	}
	entry := l.stack(stack)
	if entry.Automations == nil {
		entry.Automations = map[string]LockAutomation{}
	}
	// Read-edit-store rather than a fresh value, so the forward-compatibility
	// sidecar survives — a value rebuilt from scratch is exactly how the keys a
	// newer CLI wrote get dropped. Only for the SAME row: a path rebound to a
	// different automation keeps nothing, by the invariant SetBinding enforces at
	// the other place a rebinding can happen.
	automation := entry.Automations[path]
	if automation.AutomationID != automationID {
		automation = LockAutomation{}
	}
	automation.AutomationID, automation.UpdatedAt = automationID, updatedAt
	entry.Automations[path] = automation
	l.Stacks[stack] = entry
}

// stack returns one entry to mutate, creating the map on first write. A map
// value is not addressable, so every writer above reads, edits and stores back;
// this is the shared first half of that.
func (l *Lock) stack(name string) LockStack {
	if l.Stacks == nil {
		l.Stacks = map[string]LockStack{}
	}
	return l.Stacks[name]
}
