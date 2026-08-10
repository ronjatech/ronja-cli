package config

// The credential store's write lock.
//
// Split from config.go because it is a self-contained mechanism with nothing to
// say about profiles: an O_EXCL lock file, a nonce that makes takeover safe, and
// the eviction protocol that keeps two writers from both believing an abandoned
// lock is theirs. What it PROTECTS is one line in config.go (Update); how it
// protects it is everything below.

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

// lockWait bounds how long a mutation waits for a concurrent one. Two logins
// racing each other finish in milliseconds, so this only has to cover the
// pathological case, not the normal one.
const lockWait = 5 * time.Second

// lockStale is how old a lock must be before it is presumed abandoned, and it
// is deliberately NOT lockWait.
//
// Sharing one constant meant any holder whose work outran the wait bound got
// taken over WHILE STILL RUNNING — and the holder's work is a read-modify-write
// of a file that can live on a network home directory, on a laptop that just
// came back from sleep, or under a debugger. Those legitimately take seconds.
// A stale lock only has to be collected before it annoys a human, so an order
// of magnitude of headroom costs nothing and removes the whole class.
const lockStale = 2 * time.Minute

// lockPoll is how often a waiter re-tries the O_EXCL acquire.
const lockPoll = 20 * time.Millisecond

// withLock serializes a read-modify-write of the credential store.
//
// Every mutation (see Update) Loads the whole file, changes one entry and Saves
// it back. Two concurrent `ronja login` runs against different instances —
// or different organizations on ONE instance — would therefore read the same
// snapshot and the later Save would drop the other's entry, losing a token that
// exists on the server, was displayed nowhere, and cannot be recovered.
//
// The lock also has to cover NAME ALLOCATION, not just the write: two fresh
// logins that derive the same provisional name must not both take it, or one
// silently overwrites the other. That is why Update hands the caller the loaded
// File and runs their whole read-decide-write inside the lock.
//
// An O_EXCL lock file is the whole mechanism: atomic on every platform the CLI
// ships to, and no dependency (a real advisory lock would mean golang.org/x/sys
// or cgo). A lock older than lockStale is taken over, so a killed process
// cannot wedge logins permanently.
//
// The lock file CONTENTS are what make that takeover safe. Every holder writes
// a unique nonce, and both the takeover and the release only ever remove a file
// that still carries the nonce they expect. Without that, the lock admitted
// more writers than it excluded: `Stat`-says-stale and `Remove` are decisions
// about two different moments, so a second evictor deleted the FIRST evictor's
// brand-new lock and both ran; and a taken-over process's unconditional
// `defer os.Remove` then deleted its successor's lock, admitting a third.
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
		// Abandoned lock: take it over. Whether or not this process is the one
		// that evicts it, the next loop iteration re-tries the acquire — so a
		// lost eviction simply waits, and this cannot spin.
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

// tryAcquireLock attempts one O_EXCL create, writing body so the holder is
// identifiable. It reports whether the lock is now held by this process.
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
		// A lock whose contents were never written cannot be released by the
		// content check, so it would wedge every login for lockStale. Drop it
		// and report, rather than hold something unidentifiable.
		_ = os.Remove(lockPath)
		if writeErr != nil {
			return false, fmt.Errorf("write lock %s: %w", lockPath, writeErr)
		}
		return false, fmt.Errorf("close lock %s: %w", lockPath, closeErr)
	}
	return true, nil
}

// releaseLock removes the lock ONLY if it still carries this process's body.
//
// The unconditional remove this replaces was the second half of the takeover
// bug: a process that had been taken over still deleted the lock on its way
// out, which by then belonged to its successor.
func releaseLock(lockPath, body string) {
	if current, err := os.ReadFile(lockPath); err != nil || string(current) != body {
		return
	}
	_ = os.Remove(lockPath)
}

// takeOverStaleLock evicts a lock older than lockStale, reporting whether this
// process performed the eviction.
//
// The eviction RIGHT is claimed with an O_EXCL create on a path derived from
// the DEAD HOLDER's own nonce, so every process that judged the same holder
// stale collides on that one path and exactly one proceeds. That claim is the
// fix for the demonstrated two-writer race: without it, two waiters both Stat a
// stale lock, both Remove, and the second one's Remove deletes the first one's
// freshly created lock — after which both O_EXCL creates succeed and both run
// the critical section.
//
// One residual window is accepted rather than closed: if the stale holder wakes
// up and releases, and a third process acquires, both inside the microseconds
// between the re-read and the Remove below, that third lock is destroyed. It
// requires a process wedged for lockStale to come back to life at that exact
// instant; closing it properly needs an OS-level advisory lock, which is the
// dependency this whole mechanism exists to avoid.
func takeOverStaleLock(lockPath string) bool {
	info, err := os.Stat(lockPath)
	if err != nil || time.Since(info.ModTime()) <= lockStale {
		return false
	}
	held, err := os.ReadFile(lockPath)
	if err != nil {
		return false
	}

	// Confirm the bytes just read belong to the SAME file generation that was
	// judged stale above. Stat and ReadFile are two syscalls with a gap
	// between them, and a takeover landing in that gap yields the new
	// holder's content paired with the old staleness verdict — after which
	// evictObservedLock, which trusts the caller's observation by design,
	// re-reads that same fresh content, matches it, and removes a lock
	// created milliseconds ago. Both writers then run the critical section,
	// which is the overlap TestWithLockStaleTakeoverSerializes reports.
	//
	// An unchanged ModTime is what makes the pair atomic enough: any takeover
	// replaces the file, and the replacement is necessarily newer than a lock
	// already older than lockStale.
	if !sameStaleGeneration(lockPath, info) {
		return false
	}
	return evictObservedLock(lockPath, string(held))
}

// sameStaleGeneration reports whether lockPath is still the same file
// generation the caller saw as `seen`, and is still stale.
//
// Split out from takeOverStaleLock so the guard is directly testable. The
// interleaving it defends against — a takeover landing between a Stat and a
// ReadFile — is not reachable from outside the package, so a test that only
// races goroutines cannot tell whether this check is present: the racing test
// passes 150 consecutive runs either way and only fails under CI load. This
// function is the seam that makes the invariant assertable instead of hoped
// for.
func sameStaleGeneration(lockPath string, seen os.FileInfo) bool {
	after, err := os.Stat(lockPath)
	return err == nil &&
		after.ModTime().Equal(seen.ModTime()) &&
		time.Since(after.ModTime()) > lockStale
}

// evictObservedLock removes the lock ONLY if it still holds exactly the content
// the caller observed when it decided the lock was stale.
//
// Split out from takeOverStaleLock so the ordering that produced the two-writer
// bug is directly expressible: two waiters that both observed the same stale
// content, with the first one's eviction and re-acquire landing in between.
func evictObservedLock(lockPath, held string) bool {
	claimPath := lockPath + ".takeover." + evictionKey(held)
	claim, err := os.OpenFile(claimPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// Either another process is already evicting this exact holder, or the
		// directory is unwritable. Either way, wait — except that a claim is
		// itself a file a killed process can leave behind, and one abandoned
		// claim would otherwise make its holder permanently un-evictable. Age
		// it out on the same threshold so the wedge is bounded, not forever.
		if info, statErr := os.Stat(claimPath); statErr == nil &&
			time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(claimPath)
		}
		return false
	}
	claim.Close()
	defer os.Remove(claimPath)

	// Re-read under the claim: the holder may have released in the meantime, or
	// another evictor may already have taken over and a NEW holder may be
	// running right now. Either way the file no longer carries the content this
	// process judged stale, so it is not this process's to remove.
	current, err := os.ReadFile(lockPath)
	if err != nil || string(current) != held {
		return false
	}
	return os.Remove(lockPath) == nil
}

// newLockNonce is the per-acquisition identity written into the lock file.
// crypto/rand rather than a counter or a PID: a PID is reused, and two
// processes reusing one would each believe the other's lock was theirs to
// remove.
func newLockNonce() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate lock nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// lockBody is what a holder writes. The PID is diagnostics only — it is there
// so a human staring at a wedged lock can check whether that process is still
// alive — and the nonce is the part every comparison actually uses.
func lockBody(nonce string) string {
	return fmt.Sprintf("pid=%d nonce=%s\n", os.Getpid(), nonce)
}

// evictionKey turns a lock file's contents into a filename-safe key that every
// would-be evictor of that same holder computes identically.
//
// A lock written by an older CLI (or truncated by a crash mid-write) has no
// nonce; those all collapse to one shared key, which is still correct — it just
// means the single-evictor guarantee spans every unidentifiable lock rather
// than one specific holder.
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
