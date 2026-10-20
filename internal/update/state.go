package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ronjatech/ronja-cli/internal/config"
)

// ErrStateNotOwned is what SaveState reports instead of writing when the
// effective uid is 0 and the config directory belongs to somebody else. Callers
// treat it like any save failure; it exists so a declined write is not a nil.
var ErrStateNotOwned = errors.New("update state directory belongs to another user")

// Geteuid is os.Geteuid as a test seam for the sudo refusal (also used by
// internal/commands tests); shipping code never assigns it.
var Geteuid = os.Geteuid

// StateFileName is the update-check cache beside the credential store; it holds
// no secret.
const StateFileName = "update-check.json"

// State is what the last check learned.
type State struct {
	// CheckedAt is when the mirror was last asked, successfully or not.
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	// NotifiedAt is when the user was last told about a newer release.
	NotifiedAt time.Time `json:"notifiedAt,omitzero"`
	// Latest is the newest release known at CheckedAt.
	Latest string `json:"latest,omitempty"`
}

func statePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, StateFileName), nil
}

// LoadState reads the cache. A corrupt file reads as empty, like a missing one:
// losing a cache costs one request, while a parse error would fail a command.
func LoadState() (State, error) {
	path, err := statePath()
	if err != nil {
		return State{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, nil
	}
	return s, nil
}

// SaveState writes the cache atomically (temp file, fsync, rename). The
// directory is deliberately not fsynced: a lost rename after a crash is only a
// cache miss.
//
// ⚠️ Under sudo on somebody else's config directory it writes nothing and
// returns ErrStateNotOwned: a root-owned file there would break every later
// unprivileged run.
func SaveState(s State) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if skipStateWrite(dir) {
		return ErrStateNotOwned
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode update state: %w", err)
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(dir, "update-check-*.json")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// skipStateWrite reports root running against a config directory owned by
// someone else. A missing directory is judged by its nearest existing ancestor,
// since `sudo -E` keeps HOME pointing at the user's home and MkdirAll would
// create it root-owned there.
func skipStateWrite(dir string) bool {
	if Geteuid() != 0 {
		return false
	}
	for p := filepath.Clean(dir); ; {
		if uid, ok := dirOwnerUID(p); ok {
			return uid != 0
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}
