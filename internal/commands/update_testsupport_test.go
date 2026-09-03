package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/update"
)

// The harness the `ronja update` and daily-notice tests share: a fake mirror, a
// tarball builder, a stand-in for the installed binary, and the two seams that
// point the command at both. Split out from update_test.go because the tests
// themselves are the interesting part and were being read past three hundred
// lines of scaffolding.
//
// The tests run the real command against this fake mirror and a real file on
// disk. Two seams make that possible and nothing else is faked: releaseBaseURL
// points the release lookup at an httptest server, and currentExecutable points
// the swap at a temp file standing in for an installed ronja. Everything in
// between — the checksum, the tar reader, the probe, the rename — is the
// shipping code path.

const (
	testCurrentVersion = "v0.28.4"
	testLatestVersion  = "v0.29.0"
	oldBinaryBytes     = "the old ronja binary\n"
)

// newBinaryBytes is what the archive's `ronja` entry holds: a recognisable
// marker followed by padding.
//
// The padding is not decoration. update.ExtractBinary refuses an entry too
// small to be a Go binary — a mis-built release that ships an empty one would
// otherwise be renamed over a working CLI, taking `ronja update` with it — so
// a two-line stand-in is exactly what the floor is there to reject. Padding it
// past the floor keeps every test on the shipping path rather than behind a
// limit lowered for the tests. It costs nothing on the wire: a megabyte of
// zeroes gzips to about a kilobyte.
var newBinaryBytes = "the new ronja binary\n" + strings.Repeat("\x00", 1<<20)

// assertBinaryReplaced checks the installed binary now holds the archive's
// entry. A helper rather than an inline comparison because a failing inline %q
// would print a megabyte of that padding into the test log.
func assertBinaryReplaced(t *testing.T, path string) {
	t.Helper()
	if body := readFile(t, path); body != newBinaryBytes {
		t.Errorf("binary = %s, want the archive's ronja entry (%d bytes)", head(body), len(newBinaryBytes))
	}
}

func head(s string) string {
	if len(s) > 64 {
		return fmt.Sprintf("%q… (%d bytes)", s[:64], len(s))
	}
	return fmt.Sprintf("%q", s)
}

// fakeMirror serves the release JSON, the checksum manifest and the archives.
type fakeMirror struct {
	srv    *httptest.Server
	tag    string
	assets map[string][]byte

	// failRelease makes the release lookup answer 500, which is what a mirror
	// that is down looks like from here.
	failRelease bool
	// truncate makes an asset request stop after the first half of the body,
	// which is what a connection dropped mid-download looks like.
	truncate bool

	// hook, if set, runs on the server's goroutine as each request arrives,
	// with the requested path. It is how a test reaches the moment BETWEEN two
	// of the command's steps — what another process would see, and do, while a
	// download is in flight.
	hook func(path string)

	mu       sync.Mutex
	paths    []string
	dirSeen  []string // directory listing taken mid-download, if watchDir is set
	watchDir string
}

func newFakeMirror(t *testing.T, tag string, archive []byte) *fakeMirror {
	t.Helper()
	m := &fakeMirror{tag: tag, assets: map[string][]byte{}}
	asset := update.AssetName(tag, runtime.GOOS, runtime.GOARCH)
	m.assets[asset] = archive
	m.assets[update.ChecksumsName] = []byte(fmt.Sprintf("%s  %s\n", sha256Hex(archive), asset))

	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.paths = append(m.paths, r.URL.Path)
		hook := m.hook
		m.mu.Unlock()
		if hook != nil {
			// Outside the lock: a hook looks at the filesystem, and one of them
			// runs the command's own sweep.
			hook(r.URL.Path)
		}

		if r.URL.Path == "/repos/"+update.Repo+"/releases/latest" {
			if m.failRelease {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write(m.releaseJSON())
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		body, ok := m.assets[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Serving the archive in two halves gives the test a moment in the
		// middle of the download to look at the binary's directory — the
		// assertion that unverified bytes never land beside the binary.
		half := len(body) / 2
		w.Write(body[:half])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		m.observe()
		if m.truncate && strings.HasSuffix(name, ".tar.gz") {
			return
		}
		w.Write(body[half:])
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMirror) observe() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watchDir == "" {
		return
	}
	entries, err := os.ReadDir(m.watchDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		m.dirSeen = append(m.dirSeen, e.Name())
	}
}

func (m *fakeMirror) releaseJSON() []byte {
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	payload := struct {
		TagName string  `json:"tag_name"`
		Assets  []asset `json:"assets"`
	}{TagName: m.tag}
	for name := range m.assets {
		payload.Assets = append(payload.Assets, asset{Name: name, URL: m.srv.URL + "/download/" + name})
	}
	body, _ := json.Marshal(payload)
	return body
}

// assetRequests counts everything that is not the release lookup — i.e. the
// bytes a command promising to change nothing must not have fetched.
func (m *fakeMirror) assetRequests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, p := range m.paths {
		if !strings.HasPrefix(p, "/repos/") {
			n++
		}
	}
	return n
}

// requestCount is every request the mirror saw, of any kind.
func (m *fakeMirror) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.paths)
}

func (m *fakeMirror) sawInBinaryDir() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.dirSeen...)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// tarball builds a release archive with the same flat layout goreleaser
// publishes: ronja alongside README.md, LICENSE and NOTICE, no wrapping
// directory. entries maps name to content; a nil content means a directory
// entry, which is one of the shapes the extractor must refuse.
func tarball(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if body == nil {
			hdr = &tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if body != nil {
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("tar body %s: %v", name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// tarballOf builds an archive from ordered, typed entries — the shapes the map
// form above cannot express: a symlink or hard link wearing the binary's name,
// and two entries claiming it.
type tarEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func tarballOf(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Typeflag: flag, Linkname: e.linkname}
		if flag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if flag == tar.TypeReg && len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("tar body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func releaseTarball(t *testing.T) []byte {
	t.Helper()
	return tarball(t, map[string][]byte{
		"ronja":     []byte(newBinaryBytes),
		"README.md": []byte("# ronja\n"),
		"LICENSE":   []byte("MIT\n"),
		"NOTICE":    []byte("notice\n"),
	})
}

// installedBinary writes a stand-in for the installed ronja and points
// currentExecutable at it.
func installedBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ronja")
	if err := os.WriteFile(path, []byte(oldBinaryBytes), 0o755); err != nil {
		t.Fatalf("write stand-in binary: %v", err)
	}
	previous := currentExecutable
	currentExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { currentExecutable = previous })
	// The command resolves symlinks and reports the resolved path, and on
	// macOS t.TempDir() sits under /var, which is a link to /private/var. So
	// hand the caller the path the command will name.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve stand-in binary: %v", err)
	}
	return resolved
}

// useMirror points the release lookup at the fake and stamps the running
// version, both restored when the test ends.
func useMirror(t *testing.T, m *fakeMirror, version string) {
	t.Helper()
	previousURL, previousVersion := releaseBaseURL, Version
	releaseBaseURL, Version = m.srv.URL, version
	t.Cleanup(func() { releaseBaseURL, Version = previousURL, previousVersion })
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
}

// runUpdateCmd runs the command tree and returns stdout, stderr and the error,
// so a test can assert both the report and the stdout-stays-empty rule.
func runUpdateCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	workdir := t.TempDir()
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, workdir, args...)
	})
	return stdout, stderr, err
}
