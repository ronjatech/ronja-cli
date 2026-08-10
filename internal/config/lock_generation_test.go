package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The guard these cover defends against an interleaving inside
// takeOverStaleLock — a takeover landing between its Stat and its ReadFile,
// after which it holds the NEW holder's content paired with the OLD staleness
// verdict and evicts a live lock.
//
// They are written against sameStaleGeneration directly rather than by racing
// goroutines because racing cannot discriminate: with the guard deleted,
// TestWithLockStaleTakeoverSerializes still passes 150 consecutive local runs
// and only fails under CI load. A test that cannot fail when the behaviour it
// describes is removed is not testing anything.

// stale writes a lock file and backdates it past the staleness threshold,
// returning the FileInfo a caller would have observed.
func stale(t *testing.T, lockPath, body string) os.FileInfo {
	t.Helper()
	if err := os.WriteFile(lockPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	old := time.Now().Add(-10 * lockStale)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatalf("age lock: %v", err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat lock: %v", err)
	}
	return info
}

func TestSameStaleGenerationAcceptsUntouchedLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "cfg.lock")
	info := stale(t, lockPath, lockBody("deadbeefdeadbeefdeadbeef"))

	if !sameStaleGeneration(lockPath, info) {
		t.Error("an abandoned lock nobody touched must remain evictable, or a " +
			"crashed process wedges every later command for lockStale")
	}
}

// The regression this exists for: another process took over between the
// caller's Stat and its ReadFile, so the file on disk is now a LIVE holder's.
func TestSameStaleGenerationRejectsReplacedLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "cfg.lock")
	info := stale(t, lockPath, lockBody("deadbeefdeadbeefdeadbeef"))

	// A takeover lands: the abandoned lock is replaced by a live one.
	if err := os.WriteFile(lockPath, []byte(lockBody("11112222333344445555")), 0o600); err != nil {
		t.Fatalf("replace lock: %v", err)
	}

	if sameStaleGeneration(lockPath, info) {
		t.Error("a lock replaced since it was observed was judged still-stale — " +
			"the caller would evict a live holder and both would run the " +
			"critical section")
	}
}

// Same generation, but the holder refreshed it — it is no longer abandoned.
func TestSameStaleGenerationRejectsRefreshedLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "cfg.lock")
	info := stale(t, lockPath, lockBody("deadbeefdeadbeefdeadbeef"))

	now := time.Now()
	if err := os.Chtimes(lockPath, now, now); err != nil {
		t.Fatalf("refresh lock: %v", err)
	}

	if sameStaleGeneration(lockPath, info) {
		t.Error("a lock touched since it was observed is not abandoned")
	}
}

func TestSameStaleGenerationRejectsVanishedLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "cfg.lock")
	info := stale(t, lockPath, lockBody("deadbeefdeadbeefdeadbeef"))

	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove lock: %v", err)
	}

	if sameStaleGeneration(lockPath, info) {
		t.Error("a lock that no longer exists cannot be this process's to evict")
	}
}
