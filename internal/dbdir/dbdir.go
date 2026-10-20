// Package dbdir is the local-only binding a migrations folder remembers: which
// managed database it targets, on which instance and organization. It lives in
// git-ignored .ronja/database.json, deliberately not a committed ronja.json
// (cli/README.md → "Managed databases").
package dbdir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// DirName is the local-only state directory, shared with the workflow commands.
const DirName = ".ronja"

// FileName is the binding file inside it.
const FileName = "database.json"

// MigrationsDir is the folder `db migrate` reads .sql files from.
const MigrationsDir = "migrations"

// Binding is .ronja/database.json, keyed by (instance, organization): a
// database id belongs to one organization, and keying by URL alone would send
// one organization's id under another's token.
type Binding struct {
	Instances []Instance `json:"instances"`
}

// Instance is one binding.
type Instance struct {
	URL        string `json:"url"`
	TenantID   string `json:"tenantID,omitempty"`
	DatabaseID string `json:"databaseID"`
}

// Key is the (instance, organization) this binding belongs to.
func (i Instance) Key() wfdir.InstanceKey {
	return wfdir.InstanceKey{URL: i.URL, TenantID: i.TenantID}
}

// Path is the binding file's location under dir.
func Path(dir string) string { return filepath.Join(dir, DirName, FileName) }

// Load reads the binding file; a missing file is an empty binding.
func Load(dir string) (*Binding, error) {
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Binding{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", Path(dir), err)
	}
	var b Binding
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(dir), err)
	}
	return &b, nil
}

// DatabaseFor returns the database id bound for want, or "" if none is. An
// ambiguous match (two organizations on one instance, no tenant known) is
// returned as an error, never guessed.
func (b *Binding) DatabaseFor(want wfdir.InstanceKey) (string, error) {
	if b == nil {
		return "", nil
	}
	i, err := wfdir.Find(b.Instances, Instance.Key, want)
	if err != nil {
		return "", err
	}
	if i < 0 {
		return "", nil
	}
	return b.Instances[i].DatabaseID, nil
}

// Save records databaseID for key, replacing an equivalent entry.
func (b *Binding) Save(dir string, key wfdir.InstanceKey, databaseID string) error {
	replaced := false
	for i := range b.Instances {
		if wfdir.Matches(b.Instances[i].Key(), key) {
			b.Instances[i].DatabaseID = databaseID
			replaced = true
			break
		}
	}
	if !replaced {
		b.Instances = append(b.Instances, Instance{
			URL: key.URL, TenantID: key.TenantID, DatabaseID: databaseID,
		})
	}

	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("encode binding: %w", err)
	}
	// wfdir.StateDir, not a join: a committed symlink at .ronja must not choose
	// where this file lands.
	stateDir, err := wfdir.StateDir(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", stateDir, err)
	}
	if err := os.WriteFile(Path(dir), append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", Path(dir), err)
	}
	// Re-asserted on every save: this can be the first file in .ronja/, and a
	// binding naming one person's organization must not be committed.
	return wfdir.WriteStateGitignore(dir)
}
