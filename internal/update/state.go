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

// ErrStateNotOwned is what SaveState reports instead of writing, when the
// effective uid is 0 and the config directory belongs to somebody else.
//
// Nothing branches on it. Both callers treat every save failure the same way:
// `ronja update` ignores them all, because a cache it could not update is no
// reason to refuse an update somebody asked for, and the daily check refuses to
// go anywhere near the network after any of them — it writes CheckedAt BEFORE
// the fetch precisely so an unwritable state file costs nothing rather than a
// request per command. What the sentinel buys is a NAME: a nil return here
// would have made "root declined to write in your directory" indistinguishable
// from "written", and the tests that hold that line assert on this identity.
var ErrStateNotOwned = errors.New("update state directory belongs to another user")

// Geteuid is os.Geteuid, as a var so the sudo refusal below can be tested at
// all: the case it exists for — root against somebody else's config directory —
// is otherwise reachable only by running the suite as root against a second
// user's home, which no test can arrange.
//
// Exported because the daily check's own tests live in internal/commands and
// need the same seam to prove that ErrStateNotOwned costs no request and prints
// nothing. Nothing in the shipping code assigns it.
var Geteuid = os.Geteuid

// StateFileName is the cache beside the credential store. It holds no secret;
// it exists so a check runs at most once a day and the notice at most once a
// day, without either needing to reach the network to find that out.
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

// statePath is the state file's location. Unexported: everything outside this
// package that needs the path builds it from config.Dir and StateFileName, and
// an exported accessor for a cache's location is API somebody would eventually
// have to keep.
func statePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, StateFileName), nil
}

// LoadState reads the cache. A missing file is a first run and a corrupt one is
// treated the same way: this is a cache, and the only thing losing it costs is
// one extra request. Surfacing a parse error would turn a stale byte on disk
// into a failing command.
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

// SaveState writes the cache atomically — temp file, fsync, rename. Not for
// secrecy (there is nothing secret here) but because a half-written file read
// back later is indistinguishable from a corrupt one, and the recovery is
// another request every single invocation.
//
// The DIRECTORY is deliberately not fsynced, which is where this stops short of
// what a durable write does: after a crash the rename may not have reached the
// disk and the file reverts to its previous contents, or to not existing. For
// this file that is the same outcome as a cache miss — one extra request to the
// mirror — and paying a directory fsync on every invocation of every command to
// avoid it would be the expensive half of durability bought for nothing.
//
// It writes nothing under `sudo` on somebody else's config directory, and says
// so with ErrStateNotOwned: writing there as root leaves a root-owned file in
// the user's own directory, and every later unprivileged run then fails to read
// or rewrite it — the user's daily notice would be permanently dead as a side
// effect of one privileged update.
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

// skipStateWrite reports the sudo case: running as root against a config
// directory that belongs to someone else.
//
// A directory that does not exist yet is the same case, which is what the walk
// is for. `sudo -E ronja update` keeps HOME and XDG_CONFIG_HOME pointing at the
// user's own home, so on a machine that has never run the CLI as root the
// config directory is MISSING rather than foreign-owned — and MkdirAll would
// then create it, and everything under it, owned by root inside a home the user
// cannot afterwards write. Every later unprivileged run fails on it, which is
// precisely the outcome the owner test exists to prevent. So the question is
// asked of the nearest ancestor that does exist: that is the directory the new
// one inherits its place from.
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
			// Walked to the filesystem root without finding anything that
			// exists, or a platform with no uid to read. Nothing is known, so
			// nothing is claimed.
			return false
		}
		p = parent
	}
}
