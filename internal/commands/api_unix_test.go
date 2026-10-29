//go:build !windows

package commands

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// A path that is readable but not a REGULAR file is not a mistake to refuse —
// it is `-d @<(gzip -c dump.sql)`, `-F file=@/dev/stdin`, a named pipe a
// producer is writing into. Those cannot be streamed (their stat size is not
// the size of what reading them yields) and cannot be re-opened for a retry, so
// they are read into memory and sent from there, exactly as `@-` is.
//
// The assertion is that the bytes ARRIVE: a check that only looked at the exit
// code would pass just as well on a request that went out empty, which is the
// failure this shape actually invites.
func TestAPISendsANonRegularFileBody(t *testing.T) {
	const payload = "a,b\n1,2\n"

	for _, tc := range []struct {
		name string
		args func(fifo string) []string
	}{
		{"data", func(fifo string) []string { return []string{"-d", "@" + fifo} }},
		{"form", func(fifo string) []string { return []string{"-F", "file=@" + fifo} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEchoServer(t)
			signInTo(t, e.URL())
			dir := t.TempDir()
			fifo := filepath.Join(dir, "pipe")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			// A FIFO's reader blocks until a writer opens it, so the content
			// has to be produced concurrently — which is exactly the situation
			// a caller is in when they point the CLI at one.
			written := make(chan error, 1)
			go func() {
				f, err := os.OpenFile(fifo, os.O_WRONLY, 0o600)
				if err != nil {
					written <- err
					return
				}
				_, err = io.WriteString(f, payload)
				written <- errors.Join(err, f.Close())
			}()

			args := append([]string{"api", "-X", "POST"}, tc.args(fifo)...)
			if _, err := runCLI(t, dir, append(args, "/api/v2/file/upload/uploads")...); err != nil {
				t.Fatalf("a readable FIFO was refused: %v", err)
			}
			if err := <-written; err != nil {
				t.Fatalf("writing the FIFO: %v", err)
			}
			if !strings.Contains(e.only().Body, payload) {
				t.Errorf("the FIFO's content never reached the instance: %q", e.only().Body)
			}
		})
	}
}

// A multipart body opens its file parts LAZILY — one at a time, closed at its
// own EOF — so the two things worth pinning are that a full read leaves nothing
// open, and that a part which cannot be opened at all still leaks nothing.
//
// Note where the failure now surfaces: at READ time rather than at Open. That
// is the deliberate cost of the laziness (checkFormPaths is what catches a bad
// path before the request is shaped), and a test that still expected an error
// from Open would be asserting the eager behaviour rather than this one.
//
// The count is of the process's descriptors that refer to the parts, and the
// SUCCESSFUL case is checked first: it proves the counter can see handles at
// all, so the failing case's "none open" is evidence rather than a tautology.
func TestFormBodyClosesWhatItOpenedWhenAPartCannotBeOpened(t *testing.T) {
	dir := t.TempDir()
	var specs []string
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = filepath.Join(dir, "part"+strconv.Itoa(i)+".csv")
		if err := os.WriteFile(paths[i], []byte("a,b\n1,2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, "file"+strconv.Itoa(i)+"=@"+paths[i])
	}
	body, _, err := buildForm(specs)
	if err != nil {
		t.Fatal(err)
	}

	rc, err := body.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Read one byte at a time, so at least one measurement lands while a part
	// is genuinely open. Reading the body whole would leave the count at zero
	// throughout and prove nothing about what laziness holds.
	held := 0
	single := make([]byte, 1)
	for {
		if n := partDescriptorCount(t, paths); n > held {
			held = n
		}
		if _, err := rc.Read(single); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read: %v", err)
		}
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if held != 1 {
		t.Fatalf("reading the body held %d descriptors at once, want exactly 1 — either the parts are still opened eagerly, or the count cannot see file handles at all",
			held)
	}
	if got := partDescriptorCount(t, paths); got != 0 {
		t.Errorf("a fully-read body left %d descriptors open — the last part was not closed at its EOF", got)
	}

	// Now the third part cannot be opened at all. The failure surfaces while
	// the body is being READ, and the parts read before it must already be
	// closed — without that, a --retry whose later attempt hits an unopenable
	// file leaks a handle per attempt, invisible until a long upload loop runs
	// out of descriptors.
	if err := os.Remove(paths[2]); err != nil {
		t.Fatal(err)
	}
	rc, err = body.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := io.Copy(io.Discard, rc); err == nil {
		t.Fatal("reading a body whose third part is gone succeeded")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close after a failed read: %v", err)
	}
	if got := partDescriptorCount(t, paths); got != 0 {
		t.Errorf("a failed read leaked %d descriptors — the parts opened before the failure were not closed", got)
	}
}

// partDescriptorCount is how many of this process's descriptors refer to one
// of paths, matched by device and inode. A count of every descriptor would see
// the test binary's own traffic too: an idle keep-alive connection an earlier
// test left in the default HTTP client closes on its own goroutine, whenever
// the scheduler gets to it, and moves a raw count by one.
//
// /dev/fd is the one portable-enough view of the descriptors (a symlink to
// /proc/self/fd on Linux, an fdesc mount on macOS). Readdirnames rather than
// os.ReadDir: ReadDir stats every entry, and on macOS stat'ing the descriptor
// the listing itself is using fails with EBADF. Each descriptor is fstat'ed by
// number, never wrapped in os.NewFile, whose finalizer would close a
// descriptor this test does not own.
func partDescriptorCount(t *testing.T, paths []string) int {
	t.Helper()
	var want []syscall.Stat_t
	for _, p := range paths {
		var st syscall.Stat_t
		err := syscall.Stat(p, &st)
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		want = append(want, st)
	}

	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("cannot count open descriptors: %v", err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		t.Skipf("cannot count open descriptors: %v", err)
	}
	n := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			t.Fatalf("/dev/fd entry %q is not a descriptor number", name)
		}
		var st syscall.Stat_t
		// EBADF is a descriptor that closed after the listing was read.
		if err := syscall.Fstat(fd, &st); errors.Is(err, syscall.EBADF) {
			continue
		} else if err != nil {
			t.Fatalf("fstat descriptor %d: %v", fd, err)
		}
		for _, w := range want {
			if st.Dev == w.Dev && st.Ino == w.Ino {
				n++
				break
			}
		}
	}
	return n
}
