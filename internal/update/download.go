package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sync"
	"time"
)

const (
	// MaxChecksums caps the manifest. Five lines of hex; 64 KiB is generous.
	MaxChecksums = 64 << 10
	// MaxAsset caps the archive and the binary inside it. The real archive is
	// ~8 MB. The cap is what stops a wrong or hostile URL from filling the
	// disk, and it applies to the DECOMPRESSED entry too — a gzip stream is
	// otherwise free to expand without bound.
	MaxAsset = 100 << 20
)

// maxBinaryBytes caps the decompressed `ronja` entry. It exists as a var rather
// than as a use of MaxAsset directly so a test can prove the boundary without
// generating a hundred megabytes; nothing outside a test ever changes it, and
// nothing reads it from the environment.
var maxBinaryBytes int64 = MaxAsset

// minBinaryBytes is the floor under the decompressed `ronja` entry.
//
// A cap alone lets the worst case through: an archive whose `ronja` entry is
// zero bytes (or a stray shell stub) passes every other check — the name is
// right, the type is right, the checksum matches, because the manifest is
// generated from whatever the pipeline built — and the rename then puts an
// unrunnable file where ronja was. At that point `ronja update` is gone too,
// so the CLI cannot repair itself and the user is left reinstalling by hand
// with nothing on screen explaining why. A refusal keeps the working binary.
//
// The real archive holds an ~8 MB Go binary and the smallest one this project
// could plausibly produce is far above a megabyte, so 1 MiB refuses the
// mis-built release without being a number that a legitimately smaller future
// build could trip over. A var for the same reason maxBinaryBytes is one: the
// tests prove the boundary without a megabyte of fixture, and nothing outside
// a test ever changes it.
var minBinaryBytes int64 = 1 << 20

// ErrBinaryTooSmall is returned when the archive's `ronja` entry is too small
// to be the binary — see minBinaryBytes.
var ErrBinaryTooSmall = errors.New(`the "ronja" entry in the archive is too small to be the binary`)

// idleReadTimeout bounds a response body that has STARTED and then stopped
// making progress. Also a var only so a test can shorten it.
var idleReadTimeout = 30 * time.Second

// ErrNoBinaryEntry is returned when a release archive does not contain the one
// entry the updater is willing to install.
var ErrNoBinaryEntry = errors.New(`no regular file named "ronja" in the archive`)

// ErrAmbiguousBinaryEntry is returned when an archive carries more than one
// entry named `ronja`.
//
// Refusing is the safe answer and "the first one wins" is not: a tar file may
// legally repeat a name, most extractors let the LAST one win, and an archive
// where the honest binary is followed by another entry of the same name is a
// shape the real release pipeline never produces. There is no reading of it
// that is obviously right, so there is nothing to install.
var ErrAmbiguousBinaryEntry = errors.New(`the archive carries more than one entry named "ronja"`)

// Fetch reads a URL into memory with a ceiling. Used for the checksum manifest,
// which is small and has to be complete before anything else happens.
//
// base is the release source the URL was named by — see requireScheme, which
// both fetchers run before reaching the network. It is a parameter rather than
// a check the caller makes because a caller that forgot it would download over
// plaintext and nothing would say so.
func Fetch(ctx context.Context, base, url string, limit int64) ([]byte, error) {
	body, err := get(ctx, base, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	// One byte past the limit, so "at the limit" and "over it" are
	// distinguishable rather than both looking full.
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", Safe(url), err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d KiB limit", Safe(url), limit>>10)
	}
	return raw, nil
}

// Download streams a URL into a private temporary file, returning that file —
// still open, rewound to the start — and the SHA-256 of what was written.
//
// The caller gets the open HANDLE rather than a path, and that is the point.
// Handing back a path would mean the extractor re-opens the archive by name
// after the hash was taken, leaving a window in which anything that can write
// the temp directory swaps the verified bytes for its own and the updater
// installs something nobody checksummed. Reading through the same descriptor
// that was hashed closes the window: the bytes extracted are the bytes hashed.
//
// The name is chosen by os.CreateTemp, so it is unpredictable and created
// O_EXCL. A fixed name in a world-writable /tmp is worse than untidy: a symlink
// pre-seeded there under that name would make this function truncate whatever
// it pointed at, with the running user's privileges.
//
// On any error the file is closed and removed, so a failed download leaves
// nothing behind. On success the caller owns both the handle and the name.
// base is the release source the URL was named by; see Fetch.
func Download(ctx context.Context, base, url string, limit int64) (*os.File, string, error) {
	body, err := get(ctx, base, url)
	if err != nil {
		return nil, "", err
	}
	defer body.Close()

	f, err := os.CreateTemp("", "ronja-update-*.tar.gz")
	if err != nil {
		return nil, "", fmt.Errorf("create a temporary file for the download: %w", err)
	}
	discard := func() {
		f.Close()
		os.Remove(f.Name())
	}

	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(body, limit+1))
	if err != nil {
		discard()
		return nil, "", fmt.Errorf("download %s: %w", Safe(url), err)
	}
	if written > limit {
		discard()
		return nil, "", fmt.Errorf("%s is larger than the %d MiB limit", Safe(url), limit>>20)
	}
	if err := f.Sync(); err != nil {
		discard()
		return nil, "", fmt.Errorf("sync %s: %w", f.Name(), err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		discard()
		return nil, "", fmt.Errorf("rewind %s: %w", f.Name(), err)
	}
	return f, hex.EncodeToString(sum.Sum(nil)), nil
}

// ExtractBinary writes the archive's `ronja` entry to dest with mode.
//
// It reads from a READER, not from a path, and the reader the caller passes is
// the same open file Download hashed — see Download for why the API is shaped
// that way rather than taking a filename.
//
// The archive goreleaser publishes is FLAT: `ronja` alongside README.md,
// LICENSE and NOTICE, no wrapping directory. So the entry is matched by exact
// name and everything else is ignored — an archive that carries `ronja` as a
// directory, a symlink, a hard link, or at a path inside one is refused rather
// than interpreted, because the only thing this function is allowed to do is
// put one known file where a running binary is about to be. A second entry
// claiming the same name is refused outright (ErrAmbiguousBinaryEntry), and one
// too small to be a Go binary is refused as well (ErrBinaryTooSmall) — see
// minBinaryBytes for why an empty entry is the worst case rather than a
// harmless one.
//
// dest is expected to exist already (it is the writability probe created in the
// binary's own directory), and is truncated in place so the later rename is
// same-filesystem and therefore atomic.
func ExtractBinary(archive io.Reader, dest string, mode os.FileMode) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("read the archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	claimed, wrote := false, false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the archive: %w", err)
		}
		// path.Clean folds the "./ronja" some tar writers emit onto "ronja";
		// anything with a directory component is not our entry.
		if path.Clean(hdr.Name) != "ronja" {
			continue
		}
		if claimed {
			return ErrAmbiguousBinaryEntry
		}
		claimed = true
		// A symlink, hard link or directory that happens to be named `ronja` is
		// a claim on the name without being a file to install, so it is counted
		// above (a later real entry is then ambiguous) and skipped here. With no
		// regular entry at all the loop ends at ErrNoBinaryEntry.
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// The header's own claim, checked before a byte is written: an entry
		// that announces more than the cap is refused without decompressing it.
		if hdr.Size > maxBinaryBytes {
			return fmt.Errorf("the ronja entry declares %d bytes, more than the %d MiB limit", hdr.Size, maxBinaryBytes>>20)
		}
		// And the floor, on the same claim and for the same reason: an entry
		// announcing nothing worth installing is refused before the probe is
		// opened, so the file being replaced is never truncated for it.
		if hdr.Size < minBinaryBytes {
			// Both sides in bytes: the floor is a var the tests lower, and a
			// message rounding it to MiB would read "at least 0 MiB" there.
			return fmt.Errorf("%w: it declares %d bytes, and the smallest ronja is %d", ErrBinaryTooSmall, hdr.Size, minBinaryBytes)
		}
		if err := writeBinary(tr, dest, mode); err != nil {
			return err
		}
		wrote = true
	}
	if !wrote {
		return ErrNoBinaryEntry
	}
	return nil
}

func writeBinary(src io.Reader, dest string, mode os.FileMode) error {
	// O_NOFOLLOW: dest is the probe this process created in the binary's own
	// directory, and a symlink planted at that name in the window since would
	// otherwise be followed — writing this file through it truncates whatever
	// it points at, with the running user's privileges. In a shared
	// /usr/local/bin that is somebody else's file.
	//
	// O_CREATE: the probe normally exists, but a download slower than
	// staleProbeAge can outlive its own file if a second `ronja update` sweeps
	// it. Recreating it is better than ending an eight-megabyte download at the
	// last step; the caller touches the probe as it goes so the sweep leaves it
	// alone in the first place.
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|openNoFollow, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", dest, err)
	}
	defer out.Close()
	// The header was already checked, but a header is a claim and this is the
	// stream: a gzip bomb whose entry announces a modest size is stopped here.
	written, err := io.Copy(out, io.LimitReader(src, maxBinaryBytes+1))
	if err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if written > maxBinaryBytes {
		return fmt.Errorf("the ronja entry is larger than the %d MiB limit", maxBinaryBytes>>20)
	}
	// The floor on the STREAM, because the header was only a claim: an entry
	// that announces eight megabytes and delivers nothing would otherwise be
	// renamed over the running binary, and there would then be no `ronja
	// update` left to fix it with. Checked before the chmod below, so a short
	// file never wears the executable bit even for the moment before the
	// caller removes it.
	if written < minBinaryBytes {
		return fmt.Errorf("%w: %d bytes arrived, and the smallest ronja is %d", ErrBinaryTooSmall, written, minBinaryBytes)
	}
	// After the bytes, not before. O_TRUNC on an existing file does not change
	// its mode, so the mode has to be set explicitly somewhere — and setting it
	// once the content is all there means the ordinary path (a probe created
	// 0600) never has a half-written file wearing the executable bit. The
	// caller has already masked away setuid/setgid.
	if err := out.Chmod(mode); err != nil {
		return fmt.Errorf("chmod %s: %w", dest, err)
	}
	// Without this the rename below only orders the metadata: the name flips
	// over while the CONTENT is still in the page cache, so a power loss
	// between the two leaves a zero-length executable where ronja used to be.
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dest, err)
	}
	return nil
}

func get(ctx context.Context, base, url string) (io.ReadCloser, error) {
	// Before the connection, not after: an asset URL the release listing named
	// over plaintext, or on somebody else's host, is refused rather than
	// fetched and then judged. Scheme first, because a URL that is wrong on
	// both counts is most usefully reported as the downgrade it is.
	if err := requireScheme(base, url); err != nil {
		return nil, err
	}
	if err := requireKnownHost(base, url); err != nil {
		return nil, err
	}
	// A cancelable child of the caller's context, because the idle-read
	// watchdog below needs something to pull: a net/http body has no per-read
	// deadline, so cancelling the request is the only way to interrupt a server
	// that answered and then went quiet.
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build request for %s: %w", Safe(url), err)
	}
	req.Header.Set("User-Agent", "ronja-cli")
	// Deliberately no Authorization header — see the package doc.
	resp, err := releaseHTTPClient().Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, Safe(url))
	}
	return newIdleReader(resp.Body, cancel, idleReadTimeout), nil
}

// idleReader fails a read that has stopped making progress.
//
// The three bounds on a download are deliberately different things.
// ResponseHeaderTimeout (on the shared client) bounds the wait for a server to
// START answering. The caller's context bounds an exchange that has a natural
// deadline — the release lookup does. Neither can bound the archive body,
// because a wall-clock cap on a multi-megabyte transfer is a bandwidth
// requirement wearing a clock's clothes: it would fail the slow link it was
// never meant to be about. What belongs on a body is PROGRESS, so that is what
// this measures — a mirror that sends headers and then goes quiet must not hold
// `ronja update` forever.
type idleReader struct {
	body    io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	// progressed records a Read that landed data after the timer had already
	// expired but before the watchdog took the lock — see watchdog.
	progressed bool
	stalled    bool
	closed     bool
}

func newIdleReader(body io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) *idleReader {
	r := &idleReader{body: body, timeout: timeout, cancel: cancel}
	// Armed while holding the lock the watchdog takes, so the field the
	// watchdog re-arms is always there by the time it can look at it.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timer = time.AfterFunc(timeout, r.watchdog)
	return r
}

// watchdog is what fires when nothing has been read for the timeout.
//
// Reset cannot unschedule a func that has already been entered, so a Read
// landing data in the same instant as the expiry cannot call this off — its
// Reset comes back false and it says so instead (progressed). Cancelling on
// that firing would kill a download that IS making progress, which is the one
// thing this watchdog must not do: it exists for a body that went quiet, and a
// body that did not go quiet is exactly the case it is not about.
func (r *idleReader) watchdog() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		// Stop cannot unschedule an already-entered func either, so the same
		// question gets asked here: re-arming after Close would leave a timer
		// alive for another full timeout with nothing left to watch.
		return
	}
	if r.progressed {
		r.progressed = false
		r.timer.Reset(r.timeout)
		return
	}
	r.stalled = true
	// Under the lock: cancelling a context calls nothing back into this type.
	r.cancel()
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.mu.Lock()
		// Once the watchdog has cancelled, re-arming the timer would only
		// promise a recovery that cannot happen. Before that, Reset's answer is
		// the one thing that distinguishes "re-armed" from "too late, the
		// watchdog is already running": false means it had expired, and this
		// Read hands the re-arming over to the watchdog rather than losing it.
		if !r.stalled && !r.timer.Reset(r.timeout) {
			r.progressed = true
		}
		r.mu.Unlock()
	}
	if err != nil {
		r.mu.Lock()
		stalled := r.stalled
		r.mu.Unlock()
		if stalled {
			// The transport's own error says "context canceled", which names
			// the mechanism and not the problem the user has.
			return n, fmt.Errorf("no data for %s: the download stalled", r.timeout)
		}
	}
	return n, err
}

func (r *idleReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.timer.Stop()
	r.mu.Unlock()
	err := r.body.Close()
	// Releases the child context whether the watchdog fired or not.
	r.cancel()
	return err
}
