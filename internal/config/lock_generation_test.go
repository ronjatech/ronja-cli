package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These test sameStaleGeneration directly because a goroutine race almost
// never hits the Stat/ReadFile interleaving it guards, and passes without it.

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

func TestSameStaleGenerationRejectsReplacedLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "cfg.lock")
	info := stale(t, lockPath, lockBody("deadbeefdeadbeefdeadbeef"))

	if err := os.WriteFile(lockPath, []byte(lockBody("11112222333344445555")), 0o600); err != nil {
		t.Fatalf("replace lock: %v", err)
	}

	if sameStaleGeneration(lockPath, info) {
		t.Error("a lock replaced since it was observed was judged still-stale — " +
			"the caller would evict a live holder and both would run the " +
			"critical section")
	}
}

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
