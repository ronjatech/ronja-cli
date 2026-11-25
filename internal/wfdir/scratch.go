package wfdir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ScratchFileName is .ronja/scratch.json: the test app `ronja app test` made
// from this folder, per instance and organization, so the next run replaces
// it instead of making another.
const ScratchFileName = "scratch.json"

// ScratchApps is the file's shape.
type ScratchApps struct {
	Apps []ScratchApp `json:"apps"`
}

// ScratchApp is one test app. Folder is the folder's absolute path when it
// was made: a copy of the folder (`cp -r`) carries the file along, and must
// not overwrite the original's test app.
type ScratchApp struct {
	URL      string `json:"url"`
	TenantID string `json:"tenantID,omitempty"`
	Folder   string `json:"folder"`
	ID       string `json:"id"`
}

// Key is the (instance, organization) the test app lives in.
func (a ScratchApp) Key() InstanceKey { return InstanceKey{URL: a.URL, TenantID: a.TenantID} }

// LoadScratchApp is the id of the test app this folder made on key, "" when
// it made none there, or when the file was made by the folder at another
// path. A missing or unreadable file is no test app: the server makes a new
// one.
func LoadScratchApp(root string, key InstanceKey) string {
	raw, err := os.ReadFile(filepath.Join(root, StateDirName, ScratchFileName))
	if err != nil {
		return ""
	}
	var file ScratchApps
	if json.Unmarshal(raw, &file) != nil {
		return ""
	}
	here := scratchFolder(root)
	for _, a := range file.Apps {
		if Matches(a.Key(), key) && a.Folder == here {
			return a.ID
		}
	}
	return ""
}

// RecordScratchApp stores id as this folder's test app on key, replacing any
// other this folder holds there.
func RecordScratchApp(root string, key InstanceKey, id string) error {
	dir, err := StateDir(root)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ScratchFileName)
	var file ScratchApps
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		// A file that does not parse is replaced: it only saves a new link.
		_ = json.Unmarshal(raw, &file)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("read %s: %w", path, err)
	}
	kept := []ScratchApp{{URL: key.URL, TenantID: key.TenantID, Folder: scratchFolder(root), ID: id}}
	for _, a := range file.Apps {
		if !Matches(a.Key(), key) {
			kept = append(kept, a)
		}
	}
	body, err := json.MarshalIndent(ScratchApps{Apps: kept}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(path, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return WriteStateGitignore(root)
}

func scratchFolder(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}
