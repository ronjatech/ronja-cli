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
// Access is the data-app counterpart of Parameters, and it is a pointer for the
// same reason: a state file written before allowlists were recorded must be
// distinguishable from one that recorded "the row allows nothing". Without that
// distinction every pre-existing folder would read as having lost an allowlist
// it never knew about, and the first push would report drift it cannot explain.
type InstanceState struct {
	SourceID          string                   `json:"sourceID"`
	SourceLifecycle   string                   `json:"sourceLifecycle,omitempty"`
	BaselineUpdatedAt string                   `json:"baselineUpdatedAt,omitempty"`
	Title             string                   `json:"title,omitempty"`
	Entrypoint        string                   `json:"entrypoint,omitempty"`
	Parameters        *[]api.WorkflowParameter `json:"parameters,omitempty"`
	Access            *api.DataAppAccess       `json:"access,omitempty"`
	Files             map[string]FileState     `json:"files"`
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
func SaveState(root string, s *State) error {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := writeAtomic(StatePath(root), append(body, '\n'), 0o644); err != nil {
		return err
	}
	return WriteStateGitignore(root)
}

// WriteStateGitignore drops a "*" .gitignore inside .ronja/, so the directory
// excludes itself no matter what the surrounding repo's rules are.
func WriteStateGitignore(root string) error {
	path := filepath.Join(root, StateDirName, GitignoreName)
	return writeAtomic(path, []byte("*\n"), 0o644)
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
