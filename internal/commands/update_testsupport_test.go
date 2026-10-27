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

// The harness for the `ronja update` and daily-notice tests. Only two seams are
// faked, releaseBaseURL and currentExecutable; the checksum, tar reader, probe
// and rename are the shipping code path.

const (
	testCurrentVersion = "v0.28.4"
	testLatestVersion  = "v0.29.0"
	oldBinaryBytes     = "the old ronja binary\n"
)

// newBinaryBytes is padded past update.ExtractBinary's minimum-size floor so
// tests stay on the shipping path; a megabyte of zeroes gzips to a kilobyte.
var newBinaryBytes = "the new ronja binary\n" + strings.Repeat("\x00", 1<<20)

// assertBinaryReplaced exists so a failing comparison does not print a
// megabyte of padding into the test log.
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

type fakeMirror struct {
	srv    *httptest.Server
	tag    string
	assets map[string][]byte

	failRelease bool
	truncate    bool

	// hook runs on the server's goroutine as each request arrives, so a test can
	// act between two of the command's steps.
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
			// Outside the lock: one hook runs the command's own sweep.
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
		// Two halves give the test a moment mid-download to check that unverified
		// bytes never land beside the binary.
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

// tarball uses goreleaser's flat layout; a nil content is a directory entry,
// one of the shapes the extractor must refuse.
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

// tarballOf expresses what the map form cannot: links wearing the binary's
// name, and two entries claiming it.
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

func installedBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ronja")
	if err := os.WriteFile(path, []byte(oldBinaryBytes), 0o755); err != nil {
		t.Fatalf("write stand-in binary: %v", err)
	}
	previous := currentExecutable
	currentExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { currentExecutable = previous })
	// The command reports the symlink-resolved path, and macOS's t.TempDir() is
	// under /var, a link to /private/var.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve stand-in binary: %v", err)
	}
	return resolved
}

// Tests take the Unix branches unless they call pretendWindows: the swap they
// drive is the Unix path, and Windows refuses before it.
func init() { hostOS = "linux" }

// pretendWindows takes the Windows branches on whatever OS runs the test.
func pretendWindows(t *testing.T) {
	t.Helper()
	previous := hostOS
	hostOS = "windows"
	t.Cleanup(func() { hostOS = previous })
}

func useMirror(t *testing.T, m *fakeMirror, version string) {
	t.Helper()
	previousURL, previousVersion := releaseBaseURL, Version
	releaseBaseURL, Version = m.srv.URL, version
	t.Cleanup(func() { releaseBaseURL, Version = previousURL, previousVersion })
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
}

func runUpdateCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	workdir := t.TempDir()
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, workdir, args...)
	})
	return stdout, stderr, err
}
