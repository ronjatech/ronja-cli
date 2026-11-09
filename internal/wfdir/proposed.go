package wfdir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ProposedFileName is .ronja/proposed.json: what this folder's last change
// lists proposed. A list deploys a copy of the folder, so the folder's own
// anchors never learn what it put live; the drift guards read this instead.
const ProposedFileName = "proposed.json"

// MaxProposals bounds the history per instance: an older list can still be
// approved after a newer one was proposed.
const MaxProposals = 5

// Proposed is the file's shape, newest proposal first.
type Proposed struct {
	Proposals []Proposal `json:"proposals"`
}

// Proposal is one change list's copy of this folder, as the drift guards
// compare it: each file's sha256, and the manifest it carried. Keyed like the
// baseline it stands beside, by (instance, organization).
type Proposal struct {
	URL      string            `json:"url"`
	TenantID string            `json:"tenantID,omitempty"`
	ListID   string            `json:"listID"`
	Files    map[string]string `json:"files"`
	Manifest json.RawMessage   `json:"manifest,omitempty"`
	// Over is BaselineDigest of the baseline the folder proposed over. The
	// proposal counts only while the folder's baseline is still that one, so
	// any sync that records a new baseline retires it.
	Over string `json:"over,omitempty"`
}

// BaselineDigest is the sha256 of one instance's baseline, every field the
// drift guards compare included. Empty for no baseline, which no proposal is
// ever over.
func BaselineDigest(s *InstanceState) string {
	if s == nil {
		return ""
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return HashString(string(raw))
}

// ParsedManifest is the manifest the list carried, nil when it recorded none
// or one that does not parse.
func (p Proposal) ParsedManifest() *Manifest {
	var m Manifest
	if len(p.Manifest) == 0 || json.Unmarshal(p.Manifest, &m) != nil {
		return nil
	}
	return &m
}

// Key is the (instance, organization) the list was proposed to.
func (p Proposal) Key() InstanceKey { return InstanceKey{URL: p.URL, TenantID: p.TenantID} }

// LoadProposals reads the history this folder recorded for one instance. A
// missing or unreadable file is no history: it only ever excuses drift.
func LoadProposals(root string, key InstanceKey) []Proposal {
	raw, err := os.ReadFile(filepath.Join(root, StateDirName, ProposedFileName))
	if err != nil {
		return nil
	}
	var p Proposed
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	var out []Proposal
	for _, entry := range p.Proposals {
		if Matches(entry.Key(), key) {
			out = append(out, entry)
		}
	}
	return out
}

// RecordProposal adds p as the newest proposal for its instance, keeping
// MaxProposals per instance.
func RecordProposal(root string, p Proposal) error {
	dir, err := StateDir(root)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ProposedFileName)
	var file Proposed
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		// A file that does not parse is replaced: it is only a history.
		_ = json.Unmarshal(raw, &file)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("read %s: %w", path, err)
	}
	kept := []Proposal{p}
	same := 1
	for _, entry := range file.Proposals {
		if Matches(entry.Key(), p.Key()) {
			if same == MaxProposals {
				continue
			}
			same++
		}
		kept = append(kept, entry)
	}
	body, err := json.MarshalIndent(Proposed{Proposals: kept}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(path, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return WriteStateGitignore(root)
}
