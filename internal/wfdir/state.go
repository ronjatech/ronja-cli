package wfdir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
)

// State is .ronja/state.json — the local sync baseline, never committed.
//
// Keyed like the manifest, by (instance, organization), because the same folder
// can be bound to staging and production — or to two organizations on one
// instance — and each has its own idea of what "last synced" means.
type State struct {
	Instances []InstanceBaseline `json:"instances"`
}

// InstanceBaseline is one binding's baseline. It mirrors Manifest.Instance so
// both files can share the lookup in Find.
type InstanceBaseline struct {
	URL      string         `json:"url"`
	TenantID string         `json:"tenantID,omitempty"`
	State    *InstanceState `json:"state"`
}

// Key is the (instance, organization) this baseline belongs to.
func (i InstanceBaseline) Key() InstanceKey {
	return InstanceKey{URL: i.URL, TenantID: i.TenantID}
}

// InstanceState is what one instance's baseline records.
//
// SourceID names the ROW the files came from, which is not the same as the
// manifest's WorkflowID: a clone prefers the caller's open draft when there is
// one, so the baseline may describe a draft row while the binding names the
// live workflow it belongs to. SourceLifecycle keeps that distinction legible
// without another round-trip.
//
// Title and Entrypoint are the row's METADATA as the server last had it, and
// they are here for the same reason the file hashes are: without them a push
// sends the manifest's title and entrypoint blind, so a colleague renaming the
// workflow in the web builder is silently reverted by the next sync. They are
// omitempty because a state file written before they existed must keep parsing
// — and an absent one simply means no metadata guard, which is what a folder
// that has never recorded them deserves.
// Parameters is the same guard for the declared parameter set: a colleague
// adding one in the web builder must not be silently reverted by the next push.
// Pointer for the same reason it is one on the Manifest — a state file written
// before parameters were recorded has to be distinguishable from one that
// recorded "the row declares none", or every pre-existing folder would read as
// having lost a parameter set it never knew about.
// ReportingTimezone is the same guard for the declared execution calendar: a
// colleague changing the workflow's zone in the web builder must not be silently
// reverted by the next push. Pointer for the reason the others are — a state
// file written before the zone was recorded (nil) has to be distinguishable from
// one that recorded "the row declares none" (a pointer to ""), or every
// pre-existing folder would read as having lost a declaration it never knew
// about and the guard would fire on a difference nobody made.
// Access is the data-app counterpart of Parameters, and it is a pointer for the
// same reason: a state file written before allowlists were recorded must be
// distinguishable from one that recorded "the row allows nothing". Without that
// distinction every pre-existing folder would read as having lost an allowlist
// it never knew about, and the first push would report drift it cannot explain.
type InstanceState struct {
	SourceID          string `json:"sourceID"`
	SourceLifecycle   string `json:"sourceLifecycle,omitempty"`
	BaselineUpdatedAt string `json:"baselineUpdatedAt,omitempty"`
	Title             string `json:"title,omitempty"`
	// Name is a MODULE's Python package name as the server last had it, and it
	// is the same guard Title is: a colleague renaming the package in the web
	// UI must not be silently reverted by the next push. omitempty because
	// every other kind's baseline has no such field and must not grow an empty
	// one — and because an absent value means NO GUARD, which is what a state
	// file written before modules existed deserves.
	Name              string                   `json:"name,omitempty"`
	Entrypoint        string                   `json:"entrypoint,omitempty"`
	Parameters        *[]api.WorkflowParameter `json:"parameters,omitempty"`
	ReportingTimezone *string                  `json:"reportingTimezone,omitempty"`
	Access            *api.DataAppAccess       `json:"access,omitempty"`
	Files             map[string]FileState     `json:"files"`
	// Tables is a PIPELINE folder's per-file remote state: relative path →
	// which row the baseline describes, and whether this checkout holds an open
	// draft of it.
	//
	// Separate from Files rather than folded into FileState because the two
	// answer different questions and are written at different moments: Files is
	// "what were the bytes at the last sync" (the drift baseline), Tables is
	// "which rows are these files, and where is my draft" (so push can resume
	// its own draft without a round trip and status can say "draft staged"
	// without one either).
	//
	// omitempty and three-state-safe: ABSENT means this folder does not track
	// table state — the ordinary reading for every workflow and data-app
	// baseline, and for a pipeline folder written before the key existed. It
	// never means "no tables", which is what a present-but-empty map says.
	Tables map[string]TableState `json:"tables,omitempty"`
	// TableDocs is a PIPELINE folder's per-DOCS-SIDECAR state, and it is the
	// LEGACY half of the pair Lock.TableDocs owns. A folder on a NAMED STACK
	// keeps its sidecar recordings in the committed lock, where a fresh CI
	// checkout can read them; a legacy unnamed instances[] folder has nowhere
	// committed to put one, so it keeps them here exactly as it kept everything
	// else before stacks existed. The fork is spelled once, in the commands
	// package's liveHashes.
	//
	// omitempty and three-state-safe, for the reason Tables is: ABSENT means this
	// folder tracks no sidecar state, never "there are no sidecars".
	TableDocs map[string]TableDocsState `json:"tableDocs,omitempty"`
	// Metrics is a PIPELINE folder's per-METRIC-FILE state, and it is the LEGACY
	// half of the pair Lock.Metrics owns — the same fork, for the same reason,
	// as TableDocs above: a folder on a NAMED STACK keeps its metric recordings
	// in the committed lock, where a fresh CI checkout can read them, and a
	// legacy unnamed instances[] folder has nowhere committed to put one. The
	// fork is spelled once, in the commands package's liveHashes.
	//
	// omitempty and three-state-safe, for the reason Tables is: ABSENT means this
	// folder tracks no metric state, never "there are no metrics".
	Metrics map[string]MetricState `json:"metrics,omitempty"`
}

// MetricState is one metric file's recorded state in a LEGACY folder's local
// baseline — the mirror of wfdir.LockMetric, and it obeys the same invariant:
// A HASH IS ONLY EVER COMPARED AGAINST THE ROW IT WAS TAKEN FROM. MetricID says
// which row that was, so a metric file rebound to a different row drops the
// fingerprint with it.
type MetricState struct {
	MetricID string `json:"metricID"`
	// LiveSHA256 is the LIVE metric's recipe fingerprint — see
	// LockMetric.LiveSHA256 for why it is taken over the row's canonicalized
	// recipe and never over the file.
	LiveSHA256 string `json:"liveSHA256,omitempty"`
	// DeclaredSHA256 is what the FILE declared at the last push — see
	// LockMetric.DeclaredSHA256 for why it is a second fingerprint rather than
	// the same one.
	DeclaredSHA256 string `json:"declaredSHA256,omitempty"`
}

// TableDocsState is one docs sidecar's recorded state in a LEGACY folder's local
// baseline — the mirror of wfdir.LockTableDocs, and it obeys the same invariant:
// A HASH IS ONLY EVER COMPARED AGAINST THE ROW IT WAS TAKEN FROM. TableID says
// which row that was, so a sidecar rebound to a different table drops the
// fingerprint with it.
type TableDocsState struct {
	TableID    string `json:"tableID"`
	MetaSHA256 string `json:"metaSHA256,omitempty"`
	// DeclaredSHA256 is what the FILE declared at the last push — see
	// LockTableDocs.DeclaredSHA256 for why it is a second fingerprint rather
	// than the same one.
	DeclaredSHA256 string `json:"declaredSHA256,omitempty"`
}

// TableState is one pipeline file's remote identity as this checkout last saw
// it, plus the fingerprints of the two REMOTE ROWS that file is synced with.
//
// The invariant the two hashes exist for, and the one to keep true when adding
// a third: A HASH IS ONLY EVER COMPARED AGAINST THE ROW IT WAS TAKEN FROM. A
// pipeline file is synced with three different things, and folding them into one
// fingerprint was a real bug rather than a tidiness question — the single value
// meant "the bytes I wrote into my draft" after a push and "the bytes the live
// table holds" after a clone or a publish, while the drift guard compared BOTH
// rows against it. An ordinary push → edit → push was then refused as drift on a
// live table that had never moved.
//
//	InstanceState.Files[path].SHA256  the last COMPLETE push (written AND built).
//	                                  What "changed since the last sync" means,
//	                                  for `status` and for what a bare push sends.
//	TableState.DraftSHA256            what this checkout last wrote into DraftID,
//	                                  in the DISK form — the file's own bytes,
//	                                  aliases and sibling stems unresolved. Leg
//	                                  (b) of the drift guard compares the draft
//	                                  against this, after de-aliasing the row.
//	TableState.DraftWireSHA256        the same write, in the WIRE form — the exact
//	                                  bytes that went over the PUT, ids
//	                                  substituted for every alias and stem. This
//	                                  is what the ROW stores, so it is the only
//	                                  one of the three that may be sent as the
//	                                  server's baseCodeSha256 precondition.
//	TableState.LiveSHA256             the LIVE row's SQL as of the last moment
//	                                  this folder agreed with it — a clone, the
//	                                  fork of a draft, a publish. Leg (a)
//	                                  compares the live table against this.
//
// TableID is the LIVE row those hashes are fingerprints OF. It duplicates the
// manifest's Binding.Tables entry deliberately: the baseline has to say WHICH
// ROW it describes, not merely that it describes one. Without it a manifest
// edited to point a path at a different table — by hand, by a merge, by a second
// clone — would silently inherit the previous table's hashes, and the next push
// would compare a colleague's table against a baseline taken from something else
// entirely and conclude nothing had changed. `pipeline push` checks it against
// the binding before it writes anything, so the rebinding is DETECTED rather
// than trusted.
//
// DraftID is the caller's OWN open draft of that table, empty when there is
// none. It is local-only for the reason the whole baseline is: drafts are
// per-user, and a colleague inheriting this id from git would be pointed at a
// row they cannot see. It is a HINT, never authority — a draft can be committed
// or discarded from the web UI between two commands, so a caller re-reads
// (GET /feature/model/:id/draft) rather than trusting a recorded id blindly.
//
// DraftWireSHA256 is LOCAL-ONLY, for exactly the reason DraftID and
// DraftSHA256 are and LiveSHA256 is not: it is a fingerprint of a PER-USER
// draft row. "The live table held these bytes" is a fact about the environment
// and belongs in the committed lock; "my own open draft holds these bytes" is
// true for one person on one machine, and a colleague inheriting it from git
// would be handed a precondition taken from a row they cannot even read.
//
// It sits beside DraftSHA256 rather than replacing it because they are
// fingerprints of two different things and the invariant above forbids folding
// them: the drift guard compares the DISK form (it has to explain drift in
// terms of the folder, and it de-aliases the remote row to do so), while the
// server's precondition compares the WIRE form (that is what the row stores).
// Sending the disk hash as a precondition would 409 every push from any folder
// that spells a ref as a sibling's stem or a declared alias — which is the
// ordinary pipeline folder.
//
// All three hashes are omitempty and three-state-safe. EMPTY means "no baseline
// for that row", which disarms its leg of the drift guard exactly the way an
// absent file baseline does — the state of a folder cloned from git, of a draft
// this checkout never wrote to, and of a baseline written before these fields
// existed. For DraftWireSHA256 empty additionally means "send no precondition",
// which is the server's back-compatible "write unconditionally".
type TableState struct {
	TableID         string `json:"tableID"`
	DraftID         string `json:"draftID,omitempty"`
	DraftSHA256     string `json:"draftSHA256,omitempty"`
	DraftWireSHA256 string `json:"draftWireSHA256,omitempty"`
	LiveSHA256      string `json:"liveSHA256,omitempty"`
	// MetaSHA256 is the LIVE row's DOCUMENTATION fingerprint as of the last
	// moment this folder agreed with it — the LEGACY half of the pair
	// LockTable.MetaSHA256 owns, kept here for a folder with no named stack for
	// LiveSHA256's reason exactly.
	//
	// It is a fourth fingerprint of a fourth thing, and the invariant above
	// covers it unchanged: it is taken from the live row's prose and compared
	// against the live row's prose. Empty disarms its leg.
	MetaSHA256 string `json:"metaSHA256,omitempty"`
}

// FileState is one file as the server last had it. UpdatedAt is diagnostics
// only — drift is decided on the hash, because workflow_files.updated_at moves
// for reasons that do not change content.
type FileState struct {
	SHA256    string `json:"sha256"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// LoadState reads the baseline. A missing file is not an error — that is a
// folder someone cloned from git (whose .ronja was correctly never committed),
// or one that has never been pushed.
func LoadState(root string) (*State, error) {
	path := StatePath(root)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w (delete it to resync)", path, err)
	}
	return &s, nil
}

// SaveState writes the baseline and (re)asserts the .gitignore beside it.
//
// Writing the ignore file on every save rather than only at clone time is
// deliberate: the baseline records what one USER's draft looked like, and a
// second developer inheriting it from git would see phantom drift against a
// draft that is not theirs.
// Both writers locate the directory through StateDir rather than joining it,
// so neither can be redirected out of the folder by a committed symlink at
// .ronja — see StateDir. They are the two writes that happen on every push and
// every clone, which is why the check lives there and not at either call site.
func SaveState(root string, s *State) error {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	dir, err := StateDir(root)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, StateFileName), append(body, '\n'), 0o644); err != nil {
		return err
	}
	return WriteStateGitignore(root)
}

// WriteStateGitignore drops a "*" .gitignore inside .ronja/, so the directory
// excludes itself no matter what the surrounding repo's rules are.
func WriteStateGitignore(root string) error {
	dir, err := StateDir(root)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, GitignoreName), []byte("*\n"), 0o644)
}

// For returns one binding's baseline, or nil when it has never been synced from
// this folder.
//
// The ambiguity error Find can raise is deliberately swallowed here: a baseline
// is a sync optimisation, and "no baseline" degrades to treating every local
// file as changed — which is safe. The MANIFEST lookup for the same key reports
// the ambiguity properly, and that is the one that decides where a push lands.
func (s *State) For(key InstanceKey) *InstanceState {
	i, err := Find(s.Instances, InstanceBaseline.Key, key)
	if err != nil || i < 0 {
		return nil
	}
	return s.Instances[i].State
}

// Set records a baseline for one binding.
func (s *State) Set(key InstanceKey, inst *InstanceState) {
	if inst.Files == nil {
		inst.Files = map[string]FileState{}
	}
	url := key.URL
	if norm, err := config.NormalizeURL(url); err == nil {
		url = norm
	}
	kept := s.Instances[:0]
	for _, existing := range s.Instances {
		if !Matches(existing.Key(), key) {
			kept = append(kept, existing)
		}
	}
	s.Instances = append(kept, InstanceBaseline{URL: url, TenantID: key.TenantID, State: inst})
}

// Clear drops ONE binding's baseline, leaving every other instance's standing.
//
// The whole-file alternative — writing a fresh empty State — is a bug, not a
// shortcut: a folder bound to localhost, staging and production keeps one
// baseline each, so discarding against one instance would erase the other two.
// The next push there sees no baseline against a non-empty remote, which is
// exactly the shape the drift guard refuses, and the way out it names is
// --force: overwriting production with no comparison because a staging draft
// was thrown away.
//
// Removal rather than an empty entry, because For then answers nil — the same
// answer a folder that has never synced here gives, which is what this instance
// now is. Every local file correctly reads as added.
func (s *State) Clear(key InstanceKey) {
	kept := s.Instances[:0]
	for _, existing := range s.Instances {
		if !Matches(existing.Key(), key) {
			kept = append(kept, existing)
		}
	}
	s.Instances = kept
}

// TableStateFor returns one file's recorded table state, or the zero value when
// there is none — which is also what a nil receiver answers, since a folder that
// has never synced here has no state for any path. The zero value disarms both
// legs of the drift guard, which is the documented meaning of "no baseline".
func (i *InstanceState) TableStateFor(path string) TableState {
	if i == nil {
		return TableState{}
	}
	return i.Tables[path]
}

// Hashes flattens a baseline into path → sha256 for diffing. A nil receiver
// yields an empty map, which is the right answer: no baseline means everything
// present locally is new.
func (i *InstanceState) Hashes() map[string]string {
	out := map[string]string{}
	if i == nil {
		return out
	}
	for path, f := range i.Files {
		out[path] = f.SHA256
	}
	return out
}
