package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serveBytes answers every request with body.
func serveBytes(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type tarEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func tarGz(t *testing.T, entries ...tarEntry) []byte {
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

// probeFile stands in for the writability probe ExtractBinary writes into: a
// file that already exists in the binary's own directory.
func probeFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".ronja-update-test")
	if err := os.WriteFile(path, []byte("probe"), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	return path
}

// tempDirOfOurOwn points os.TempDir() at a directory this test can assert
// about — specifically, that a failed download left nothing in it.
func tempDirOfOurOwn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func assertNoDownloadResidue(t *testing.T, dir string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, "ronja-update-*"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(left) > 0 {
		t.Errorf("a failed download left %v behind", left)
	}
}

func shortenBinaryCap(t *testing.T, cap int64) {
	t.Helper()
	previous := maxBinaryBytes
	maxBinaryBytes = cap
	t.Cleanup(func() { maxBinaryBytes = previous })
}

// lowerBinaryFloor drops the minimum size so a test about something else can
// use a one-line "binary" instead of a megabyte of padding. The floor itself
// is proved at its SHIPPING value in
// TestExtractRefusesAnEntryTooSmallToBeABinary, which lowers nothing.
func lowerBinaryFloor(t *testing.T, floor int64) {
	t.Helper()
	previous := minBinaryBytes
	minBinaryBytes = floor
	t.Cleanup(func() { minBinaryBytes = previous })
}

// The cap has to distinguish "exactly the limit" from "one byte over", or a
// release that grows to precisely the ceiling becomes uninstallable.
func TestFetchAtAndOverTheLimit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 64)
	url := serveBytes(t, body)

	got, err := Fetch(context.Background(), url, url, int64(len(body)))
	if err != nil {
		t.Fatalf("Fetch at exactly the limit: %v", err)
	}
	if len(got) != len(body) {
		t.Errorf("Fetch returned %d bytes, want %d", len(got), len(body))
	}
	if _, err := Fetch(context.Background(), url, url, int64(len(body))-1); err == nil {
		t.Fatal("Fetch accepted a body one byte over the limit")
	}
}

// The real MaxChecksums, so the shipping constant is on the path too and not
// just the parameter.
func TestFetchHonoursMaxChecksums(t *testing.T) {
	url := serveBytes(t, bytes.Repeat([]byte("a"), MaxChecksums))
	if _, err := Fetch(context.Background(), url, url, MaxChecksums); err != nil {
		t.Fatalf("Fetch of exactly MaxChecksums bytes: %v", err)
	}
	over := serveBytes(t, bytes.Repeat([]byte("a"), MaxChecksums+1))
	if _, err := Fetch(context.Background(), over, over, MaxChecksums); err == nil {
		t.Fatal("Fetch accepted a manifest one byte over MaxChecksums")
	}
}

// The limit is a parameter, so the boundary is proved with a small one rather
// than by streaming the real 100 MiB — the arithmetic under test is the same.
func TestDownloadRefusesMoreThanTheLimit(t *testing.T) {
	tmp := tempDirOfOurOwn(t)
	url := serveBytes(t, bytes.Repeat([]byte("x"), 1025))

	f, _, err := Download(context.Background(), url, url, 1024)
	if err == nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatal("Download accepted a body one byte over the limit")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want it to name the limit", err)
	}
	assertNoDownloadResidue(t, tmp)
}

// The bound that belongs on a body is progress, not wall time: a mirror that
// answers and then goes quiet must not hold the command forever.
func TestDownloadStallsOut(t *testing.T) {
	previous := idleReadTimeout
	idleReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { idleReadTimeout = previous })

	tmp := tempDirOfOurOwn(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("the first half"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	f, _, err := Download(context.Background(), srv.URL, srv.URL, MaxAsset)
	if err == nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatal("Download succeeded against a stalled server")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Download took %s against a stalled server, want the idle bound to end it", elapsed)
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error = %q, want it to say the download stalled", err)
	}
	assertNoDownloadResidue(t, tmp)
}

// The property the whole handle-passing API exists for: what gets installed is
// what got hashed, even if the file at that PATH is replaced in between.
//
// The swap here is the real attack shape — unlink and recreate, which is what a
// symlink or a hostile file in a shared /tmp amounts to — and not a rewrite in
// place, which would change the same inode and defeat any scheme.
func TestExtractReadsTheHandleThatWasHashed(t *testing.T) {
	tempDirOfOurOwn(t)
	lowerBinaryFloor(t, 1)
	honest := tarGz(t, tarEntry{name: "ronja", body: []byte("the verified binary\n")})
	hostile := tarGz(t, tarEntry{name: "ronja", body: []byte("something else entirely\n")})

	honestURL := serveBytes(t, honest)
	f, sum, err := Download(context.Background(), honestURL, honestURL, MaxAsset)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	if want := sha256Of(honest); sum != want {
		t.Fatalf("sum = %s, want %s", sum, want)
	}

	if err := os.Remove(f.Name()); err != nil {
		t.Fatalf("remove the archive: %v", err)
	}
	if err := os.WriteFile(f.Name(), hostile, 0o600); err != nil {
		t.Fatalf("plant a replacement archive: %v", err)
	}

	dest := probeFile(t)
	if err := ExtractBinary(f, dest, 0o755); err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(body) != "the verified binary\n" {
		t.Errorf("installed %q — the extraction read the path, not the verified handle", body)
	}
}

func TestExtractRefusesTheShapesThatAreNotABinary(t *testing.T) {
	// What is under test here is the SHAPE of the archive, so the size floor is
	// lowered out of the way: it is proved at its shipping value next door, and
	// leaving it in would answer an ambiguous archive with a complaint about
	// how big its first entry is.
	lowerBinaryFloor(t, 1)
	cases := []struct {
		name    string
		entries []tarEntry
		want    error
	}{
		{
			name:    "a symlink wearing the name",
			entries: []tarEntry{{name: "ronja", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
			want:    ErrNoBinaryEntry,
		},
		{
			name:    "a hard link wearing the name",
			entries: []tarEntry{{name: "README.md", body: []byte("#\n")}, {name: "ronja", typeflag: tar.TypeLink, linkname: "README.md"}},
			want:    ErrNoBinaryEntry,
		},
		{
			name:    "a directory wearing the name",
			entries: []tarEntry{{name: "ronja", typeflag: tar.TypeDir}},
			want:    ErrNoBinaryEntry,
		},
		{
			name: "two entries claiming the name",
			entries: []tarEntry{
				{name: "ronja", body: []byte("the first\n")},
				{name: "ronja", body: []byte("the second\n")},
			},
			want: ErrAmbiguousBinaryEntry,
		},
		{
			// The shape that would otherwise slip past: a claim that is not a
			// file, followed by one that is.
			name: "a symlink followed by a real file",
			entries: []tarEntry{
				{name: "ronja", typeflag: tar.TypeSymlink, linkname: "elsewhere"},
				{name: "ronja", body: []byte("the real one\n")},
			},
			want: ErrAmbiguousBinaryEntry,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := probeFile(t)
			err := ExtractBinary(bytes.NewReader(tarGz(t, tc.entries...)), dest, 0o755)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ExtractBinary = %v, want %v", err, tc.want)
			}
		})
	}
}

// A `ronja` entry with nothing in it passes every other check: the name is
// right, the type is right, and the checksum matches, because the manifest is
// generated from whatever the pipeline built. Installing it replaces a working
// CLI with an unrunnable file and takes `ronja update` away in the same
// rename, so there is nothing left to repair the install with. At the SHIPPING
// floor, with the probe left as it was.
func TestExtractRefusesAnEntryTooSmallToBeABinary(t *testing.T) {
	cases := map[string][]byte{
		"an empty entry":        nil,
		"a shell stub":          []byte("#!/bin/sh\nexit 1\n"),
		"most of a real binary": bytes.Repeat([]byte("x"), int(minBinaryBytes)-1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dest := probeFile(t)
			err := ExtractBinary(bytes.NewReader(tarGz(t, tarEntry{name: "ronja", body: body})), dest, 0o755)
			if !errors.Is(err, ErrBinaryTooSmall) {
				t.Fatalf("ExtractBinary = %v, want ErrBinaryTooSmall", err)
			}
			if got, _ := os.ReadFile(dest); string(got) != "probe" {
				t.Errorf("the probe was written before the size was checked: %q", got)
			}
		})
	}
}

// A header is a claim, so it is checked before anything is decompressed.
func TestExtractRefusesAnOversizedHeader(t *testing.T) {
	shortenBinaryCap(t, 8)
	dest := probeFile(t)
	err := ExtractBinary(bytes.NewReader(tarGz(t, tarEntry{name: "ronja", body: []byte("far more than eight bytes\n")})), dest, 0o755)
	if err == nil {
		t.Fatal("ExtractBinary accepted an entry declaring more than the cap")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want it to name the limit", err)
	}
	if body, _ := os.ReadFile(dest); string(body) != "probe" {
		t.Errorf("the probe was written before the header was checked: %q", body)
	}
}

// And the stream is checked too, because a header is only a claim: this is the
// gzip-bomb shape, where the entry announces a modest size and keeps going.
func TestWriteBinaryRefusesAnOversizedStream(t *testing.T) {
	shortenBinaryCap(t, 16)
	dest := probeFile(t)
	err := writeBinary(strings.NewReader(strings.Repeat("x", 4096)), dest, 0o755)
	if err == nil {
		t.Fatal("writeBinary accepted a stream larger than the cap")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want it to name the limit", err)
	}
}

// The header is only a claim, so the floor is enforced on the stream as well:
// an entry that announces a whole binary and then delivers nothing must not be
// renamed over the running one — that is the case where `ronja update` itself
// disappears and there is nothing left to repair the install with.
func TestWriteBinaryRefusesAStreamBelowTheFloor(t *testing.T) {
	lowerBinaryFloor(t, 32)
	dest := probeFile(t)
	err := writeBinary(strings.NewReader("far too short"), dest, 0o755)
	if !errors.Is(err, ErrBinaryTooSmall) {
		t.Fatalf("writeBinary = %v, want ErrBinaryTooSmall", err)
	}
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// The probe sits in the binary's own directory — /usr/local/bin on a shared
// machine — and is created moments before it is written. A symlink planted at
// that name in the window between would otherwise be followed, and this write
// would truncate whatever it points at with the running user's privileges.
func TestWriteBinaryRefusesASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no O_NOFOLLOW here — the flag is zero and the guard is not available")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "somebody-elses-file")
	if err := os.WriteFile(victim, []byte("do not touch\n"), 0o600); err != nil {
		t.Fatalf("write the victim: %v", err)
	}
	planted := filepath.Join(dir, ".ronja-update-1")
	if err := os.Symlink(victim, planted); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}

	lowerBinaryFloor(t, 1)
	if err := writeBinary(strings.NewReader("the new binary\n"), planted, 0o755); err == nil {
		t.Fatal("writeBinary followed a symlink at the probe's name")
	}
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read the victim: %v", err)
	}
	if string(body) != "do not touch\n" {
		t.Errorf("the symlink's target was written through: %q", body)
	}
}

// A probe that was swept out from under a long download is recreated rather
// than ending it at the last step: O_CREATE is what makes the sweep's worst
// case a recoverable one.
func TestWriteBinaryRecreatesASweptProbe(t *testing.T) {
	lowerBinaryFloor(t, 1)
	dest := probeFile(t)
	if err := os.Remove(dest); err != nil {
		t.Fatalf("sweep the probe: %v", err)
	}
	if err := writeBinary(strings.NewReader("the new binary\n"), dest, 0o755); err != nil {
		t.Fatalf("writeBinary on a swept probe: %v", err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read the recreated probe: %v", err)
	}
	if string(body) != "the new binary\n" {
		t.Errorf("probe = %q, want the written bytes", body)
	}
}

// The watchdog is about a body that went QUIET, and a slow one has not. A
// download that keeps arriving in small pieces, each gap shorter than the idle
// bound but the whole transfer many times longer than it, must run to the end.
func TestDownloadSurvivesASlowButProgressingBody(t *testing.T) {
	previous := idleReadTimeout
	idleReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { idleReadTimeout = previous })

	tempDirOfOurOwn(t)
	body := bytes.Repeat([]byte("x"), 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, b := range body {
			w.Write([]byte{b})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()

	f, sum, err := Download(context.Background(), srv.URL, srv.URL, MaxAsset)
	if err != nil {
		t.Fatalf("Download of a slow but progressing body: %v", err)
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	if sum != sha256Of(body) {
		t.Errorf("sum = %q, want the whole body's", sum)
	}
}
