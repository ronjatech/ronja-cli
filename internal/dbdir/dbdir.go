// Package dbdir is the local-only binding a migrations folder remembers: which
// managed database it targets, on which instance and organization.
//
// WHY THIS IS NOT A COMMITTED ronja.json. The workflow folder's manifest is
// committed on purpose — it describes the workflow itself, and a colleague who
// clones the repo should get it. A migrations folder has no such content to
// share: the .sql files ARE the shareable part, and all that is left is one id
// pointing at one organization's database, which is exactly the thing that
// differs per person.
//
// It would also collide. wfdir.FindRoot walks up from the working directory
// looking for a ronja.json, and wfdir.LoadManifest refuses one whose kind is not
// "workflow". A committed ronja.json with kind "database" at a repository root
// would therefore break every `ronja wf` command run anywhere beneath it, and a
// workflow manifest sitting above a migrations/ folder would break `db migrate`.
// Sharing that file needs the manifest layer split properly, which is worth
// doing when there is something worth sharing — and is not worth coupling to
// this.
//
// So: .ronja/database.json, git-ignored like the workflow baseline beside it,
// holding nothing that cannot be re-derived by passing --database once.
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

// Binding is .ronja/database.json.
//
// Keyed by (instance, organization) through wfdir.InstanceKey — the SAME reason
// it is keyed that way for workflows applies verbatim here: a managed database
// id belongs to exactly one organization, and a person in two organizations on
// one instance would otherwise have the second overwrite the first's binding and
// then send one organization's id under the other's token.
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

// Load reads the binding file. A missing file is an EMPTY binding, not an error:
// a folder that has never been pushed is a normal state, and --database is
// always accepted.
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

// DatabaseFor returns the database id bound for want, or "" if none is.
//
// The ErrAmbiguousInstance case is passed through rather than guessed at: a
// folder bound to two organizations on one instance, consulted while signed out,
// has no right answer, and picking one would send a migration set at whichever
// database won a coin flip.
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

// Save records databaseID for key, replacing an equivalent entry rather than
// accumulating a second one beside it.
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
	stateDir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", stateDir, err)
	}
	if err := os.WriteFile(Path(dir), append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", Path(dir), err)
	}
	// Re-assert the self-excluding .gitignore, sharing the workflow commands'
	// helper because they share the directory. It has to be re-asserted rather
	// than written once: this file can be the FIRST thing to create .ronja/ in a
	// folder that has no workflow, and a binding naming one person's organization
	// is not something to commit.
	return wfdir.WriteStateGitignore(dir)
}
