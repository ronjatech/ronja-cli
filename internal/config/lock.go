package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lockWait bounds how long a mutation waits for a concurrent one.
const lockWait = 5 * time.Second

// lockStale is how old a lock must be before it is presumed abandoned. It must
// stay well above lockWait: a holder on a network home directory or a laptop
// back from sleep can legitimately take seconds, and must not be taken over
// while still running.
const lockStale = 2 * time.Minute

// lockPoll is how often a waiter re-tries the O_EXCL acquire.
const lockPoll = 20 * time.Millisecond

// withLock serializes a read-modify-write of the credential store, so two
// concurrent logins cannot drop each other's unrecoverable token or both take
// one derived name. It is an O_EXCL lock file (no x/sys or cgo dependency); a
// lock older than lockStale is taken over. Each holder writes a unique nonce,
// and takeover and release remove only a file still carrying the expected
// content. Neither the check nor the Remove is atomic, so once a holder outlives
// lockStale, a successor's lock can still be lost (see takeOverStaleLock's
// accepted residual).
func withLock(fn func() error) error {
	path, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	lockPath := path + ".lock"

	nonce, err := newLockNonce()
	if err != nil {
		return err
	}
	body := lockBody(nonce)

	deadline := time.Now().Add(lockWait)
	for {
		acquired, err := tryAcquireLock(lockPath, body)
		if err != nil {
			return err
		}
		if acquired {
			break
		}
		// Evicted or not, the next iteration re-tries the acquire; a lost
		// eviction just waits.
		if takeOverStaleLock(lockPath) {
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s — delete it if no other ronja command is running", lockPath)
		}
		time.Sleep(lockPoll)
	}
	defer releaseLock(lockPath, body)

	return fn()
}

func tryAcquireLock(lockPath, body string) (bool, error) {
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	_, writeErr := lock.WriteString(body)
	closeErr := lock.Close()
	if writeErr != nil || closeErr != nil {
		// A lock without its body can never pass the release check and would
		// wedge every login for lockStale.
		_ = os.Remove(lockPath)
		if writeErr != nil {
			return false, fmt.Errorf("write lock %s: %w", lockPath, writeErr)
		}
		return false, fmt.Errorf("close lock %s: %w", lockPath, closeErr)
	}
	return true, nil
}

// releaseLock removes the lock only if it still carries this process's body:
// after a takeover it belongs to the successor.
func releaseLock(lockPath, body string) {
	if current, err := os.ReadFile(lockPath); err != nil || string(current) != body {
		return
	}
	_ = os.Remove(lockPath)
}

// takeOverStaleLock evicts a lock older than lockStale, reporting whether this
// process performed the eviction. The right to evict is an O_EXCL claim on a
// path derived from the dead holder's nonce, so of every waiter that judged it
// stale exactly one proceeds; otherwise the second Remove deletes the first
// evictor's fresh lock and both run.
//
// Accepted residual window: a stale holder that wakes, releases, and is
// replaced by a third acquirer between the re-read and the Remove loses that
// third lock. Closing it needs an OS advisory lock, the dependency this avoids.
func takeOverStaleLock(lockPath string) bool {
	info, err := os.Stat(lockPath)
	if err != nil || time.Since(info.ModTime()) <= lockStale {
		return false
	}
	held, err := os.ReadFile(lockPath)
	if err != nil {
		return false
	}

	// The bytes read must be the generation judged stale: a takeover between
	// Stat and ReadFile would pair a fresh holder's content with the old
	// verdict, and evictObservedLock trusts that observation. Any takeover
	// replaces the file with a newer one, so an unchanged ModTime proves it.
	if !sameStaleGeneration(lockPath, info) {
		return false
	}
	return evictObservedLock(lockPath, string(held))
}

// sameStaleGeneration reports whether lockPath is still the file generation
// the caller saw as `seen`, and is still stale. A separate function so the
// guard is testable directly: a goroutine race almost never hits the
// interleaving it defends.
func sameStaleGeneration(lockPath string, seen os.FileInfo) bool {
	after, err := os.Stat(lockPath)
	return err == nil &&
		after.ModTime().Equal(seen.ModTime()) &&
		time.Since(after.ModTime()) > lockStale
}

// evictObservedLock removes the lock only if it still holds exactly the
// content the caller observed when it judged the lock stale.
func evictObservedLock(lockPath, held string) bool {
	claimPath := lockPath + ".takeover." + evictionKey(held)
	claim, err := os.OpenFile(claimPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// Another process is evicting this holder (or the directory is
		// unwritable): wait. An abandoned claim ages out on lockStale, or its
		// holder would be un-evictable forever.
		if info, statErr := os.Stat(claimPath); statErr == nil &&
			time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(claimPath)
		}
		return false
	}
	claim.Close()
	defer os.Remove(claimPath)

	// Re-read under the claim: the holder may have released, or a new holder
	// may be running.
	current, err := os.ReadFile(lockPath)
	if err != nil || string(current) != held {
		return false
	}
	return os.Remove(lockPath) == nil
}

// newLockNonce is the per-acquisition identity written into the lock file;
// random rather than a PID, which is reused.
func newLockNonce() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate lock nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// lockBody is what a holder writes. The PID is diagnostics only; comparisons
// use the nonce.
func lockBody(nonce string) string {
	return fmt.Sprintf("pid=%d nonce=%s\n", os.Getpid(), nonce)
}

// evictionKey turns a lock file's contents into a filename-safe key that every
// would-be evictor of that holder computes identically. Locks without a nonce
// share one key, which stays correct, only coarser.
func evictionKey(body string) string {
	for _, field := range strings.Fields(body) {
		if nonce, ok := strings.CutPrefix(field, "nonce="); ok && isHex(nonce) {
			return nonce
		}
	}
	return "unidentified"
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
