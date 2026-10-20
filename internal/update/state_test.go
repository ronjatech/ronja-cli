package update

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)

	want := State{
		CheckedAt:  time.Now().UTC().Truncate(time.Second),
		NotifiedAt: time.Now().UTC().Truncate(time.Second).Add(-time.Hour),
		Latest:     "v0.29.0",
	}
	if err := SaveState(want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !got.CheckedAt.Equal(want.CheckedAt) || !got.NotifiedAt.Equal(want.NotifiedAt) || got.Latest != want.Latest {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}

	// The write is temp-file-plus-rename; nothing may be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != StateFileName {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("config dir holds %v, want only %s", names, StateFileName)
	}
}

func TestSaveStateRefusesADirectoryOwnedBySomebodyElse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no uid to compare — dirOwnerUID always declines here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, so a temp directory really is root's own")
	}
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)

	// Only the effective uid is forced: the shape of `sudo ronja update`.
	previous := Geteuid
	Geteuid = func() int { return 0 }
	t.Cleanup(func() { Geteuid = previous })

	if err := SaveState(State{CheckedAt: time.Now(), Latest: "v0.29.0"}); !errors.Is(err, ErrStateNotOwned) {
		t.Fatalf("SaveState = %v, want ErrStateNotOwned", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config dir: %v", err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the refused write left %v behind, want nothing at all", names)
	}
}

// `sudo -E` with no config directory yet: MkdirAll would create it root-owned.
func TestSaveStateRefusesToCreateADirectoryUnderSomebodyElsesHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no uid to compare — dirOwnerUID always declines here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, so a temp directory really is root's own")
	}
	home := t.TempDir()
	missing := filepath.Join(home, "ronja")
	t.Setenv("RONJA_CONFIG_DIR", missing)

	previous := Geteuid
	Geteuid = func() int { return 0 }
	t.Cleanup(func() { Geteuid = previous })

	if err := SaveState(State{CheckedAt: time.Now(), Latest: "v0.29.0"}); !errors.Is(err, ErrStateNotOwned) {
		t.Fatalf("SaveState = %v, want ErrStateNotOwned", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want the directory never created", missing, err)
	}
}

func TestLoadStateMissingIsZero(t *testing.T) {
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got != (State{}) {
		t.Errorf("LoadState on a first run = %+v, want the zero value", got)
	}
}

func TestLoadStateCorruptIsZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, StateFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState surfaced an error for a corrupt cache: %v", err)
	}
	if got != (State{}) {
		t.Errorf("LoadState on a corrupt cache = %+v, want the zero value", got)
	}
}
