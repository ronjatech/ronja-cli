package commands

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/update"
)

func TestUpdateCheckReportsAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	stdout, stderr, err := runUpdateCmd(t, "update", "--check")
	if err != nil {
		t.Fatalf("update --check: %v", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty without --json", stdout)
	}
	want := fmt.Sprintf("ronja %s is available (you have %s). Would replace %s with %s",
		testLatestVersion, testCurrentVersion, binary, update.AssetName(testLatestVersion, runtime.GOOS, runtime.GOARCH))
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
	if n := mirror.assetRequests(); n != 0 {
		t.Errorf("--check made %d asset requests, want 0", n)
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("--check rewrote the binary: %q", body)
	}
}

func TestUpdateCheckJSON(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	stdout, _, err := runUpdateCmd(t, "update", "--check", "--json")
	if err != nil {
		t.Fatalf("update --check --json: %v", err)
	}
	payload := decodeJSON(t, stdout)
	assertJSONFields(t, payload, map[string]any{
		"current":  testCurrentVersion,
		"latest":   testLatestVersion,
		"upToDate": false,
		"method":   "swap",
		"path":     binary,
		"asset":    update.AssetName(testLatestVersion, runtime.GOOS, runtime.GOARCH),
	})
}

func TestUpdateSwapsTheBinary(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	if err := os.Chmod(binary, 0o755|os.ModeSetuid); err != nil {
		t.Fatalf("chmod setuid: %v", err)
	}
	// Aged past the sweep's threshold: a SIGKILLed run's leftover, not a live probe.
	stale := filepath.Join(dir, ".ronja-update-999")
	if err := os.WriteFile(stale, []byte("leftover"), 0o600); err != nil {
		t.Fatalf("plant a stale probe: %v", err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age the stale probe: %v", err)
	}

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	mirror.watchDir = dir
	useMirror(t, mirror, testCurrentVersion)

	stdout, stderr, err := runUpdateCmd(t, "update")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty without --json", stdout)
	}
	want := fmt.Sprintf("Updated ronja %s → %s at %s", testCurrentVersion, testLatestVersion, binary)
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
	assertBinaryReplaced(t, binary)

	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Error("the setuid bit survived onto a downloaded binary")
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("mode = %v, want 0755 carried over from the replaced binary", perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read binary dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "ronja" {
			t.Errorf("binary dir holds %q; want only the binary (no probe, no archive)", e.Name())
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale probe was not swept")
	}
	seen := mirror.sawInBinaryDir()
	if len(seen) == 0 {
		t.Fatal("the mid-download directory listing never ran, so the assertion below proves nothing")
	}
	for _, name := range seen {
		if strings.HasSuffix(name, ".tar.gz") {
			t.Errorf("an archive (%s) appeared in the binary's directory mid-download", name)
		}
	}

	state, err := update.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Latest != testLatestVersion {
		t.Errorf("state Latest = %q, want %q", state.Latest, testLatestVersion)
	}
	if state.CheckedAt.IsZero() {
		t.Error("state CheckedAt was not recorded")
	}
}

func TestUpdateRefusesABadChecksum(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	archive := releaseTarball(t)
	mirror := newFakeMirror(t, testLatestVersion, archive)
	asset := update.AssetName(testLatestVersion, runtime.GOOS, runtime.GOARCH)
	mirror.assets[update.ChecksumsName] = []byte(fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), asset))
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update accepted a mismatched checksum")
	}
	if !strings.Contains(err.Error(), asset) || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %q, want a checksum mismatch naming %s", err, asset)
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("binary = %q, want the original bytes untouched", body)
	}
	assertNoResidue(t, dir)
}

func TestUpdateRefusesAnArchiveWithoutARonjaEntry(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, tarball(t, map[string][]byte{
		"README.md": []byte("# ronja\n"),
		"bin/ronja": []byte(newBinaryBytes),
	}))
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update accepted an archive with no top-level ronja entry")
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("binary = %q, want the original bytes untouched", body)
	}
	assertNoResidue(t, dir)
}

func TestUpdateRefusesARonjaDirectoryEntry(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, tarball(t, map[string][]byte{
		"ronja": nil, // a directory named like the binary
	}))
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update accepted a directory entry named ronja")
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("binary = %q, want the original bytes untouched", body)
	}
	assertNoResidue(t, dir)
}

// A stub `ronja` entry passes the checksum (the manifest is generated from
// whatever the pipeline built), and installing it would also take away
// `ronja update` itself.
func TestUpdateRefusesABinaryTooSmallToBeOne(t *testing.T) {
	tmp := isolateTempDir(t)
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, tarball(t, map[string][]byte{
		"ronja":     []byte("#!/bin/sh\nexit 1\n"),
		"README.md": []byte("# ronja\n"),
	}))
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update installed an archive entry too small to be the binary")
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("error = %q, want it to say the entry is too small to be the binary", err)
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("binary = %q, want the original bytes untouched", body)
	}
	assertNoResidue(t, dir)
	assertNoArchiveResidue(t, tmp)
}

// `[` in a path is a metacharacter to filepath.Glob, and the sweep deletes what
// it matches.
func TestSweepStaleProbesInADirectoryWithAGlobCharacter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin[old]")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make bin dir: %v", err)
	}
	stale := filepath.Join(dir, ".ronja-update-999")
	if err := os.WriteFile(stale, []byte("leftover"), 0o600); err != nil {
		t.Fatalf("plant a stale probe: %v", err)
	}
	old := time.Now().Add(-3 * staleProbeAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age the stale probe: %v", err)
	}
	live := filepath.Join(dir, ".ronja-update-998")
	if err := os.WriteFile(live, []byte("a concurrent run's probe"), 0o600); err != nil {
		t.Fatalf("plant a live probe: %v", err)
	}
	keep := filepath.Join(dir, "ronja")
	if err := os.WriteFile(keep, []byte(oldBinaryBytes), 0o755); err != nil {
		t.Fatalf("write the binary: %v", err)
	}

	sweepStaleProbes(dir)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale probe survived a directory name with a glob character in it")
	}
	if body := readFile(t, live); body != "a concurrent run's probe" {
		t.Errorf("the live probe was swept: %q", body)
	}
	if body := readFile(t, keep); body != oldBinaryBytes {
		t.Errorf("the sweep touched the binary: %q", body)
	}
}

func TestUpdateDefersToHomebrew(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Caskroom", "ronja", "0.28.4")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stage cask dir: %v", err)
	}
	installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	stdout, stderr, err := runUpdateCmd(t, "update")
	if err != nil {
		t.Fatalf("update on a Homebrew install: %v, want a clean deferral", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty without --json", stdout)
	}
	if !strings.Contains(stderr, "ronja is installed by Homebrew — run: brew upgrade ronja") {
		t.Errorf("stderr = %q, want the brew instruction", stderr)
	}
	if n := mirror.assetRequests(); n != 0 {
		t.Errorf("the Homebrew path made %d asset requests, want 0", n)
	}
}

func TestUpdateRefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write to a 0500 directory")
	}
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make bin dir: %v", err)
	}
	binary := installedBinary(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update succeeded against an unwritable directory")
	}
	if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want it to name %s and say permission denied", err, dir)
	}
	if n := mirror.assetRequests(); n != 0 {
		t.Errorf("the refusal made %d asset requests, want 0", n)
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("binary = %q, want the original bytes untouched", body)
	}
}

func TestUpdateOnTheNewestReleaseSaysSo(t *testing.T) {
	dir := t.TempDir()
	installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testLatestVersion)

	stdout, stderr, err := runUpdateCmd(t, "update")
	if err != nil {
		t.Fatalf("update on the newest release: %v", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty without --json", stdout)
	}
	if want := "no release newer than ronja " + testLatestVersion; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
	if n := mirror.assetRequests(); n != 0 {
		t.Errorf("an up-to-date binary made %d asset requests, want 0", n)
	}
	state, err := update.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Latest != testLatestVersion {
		t.Errorf("state Latest = %q, want %q", state.Latest, testLatestVersion)
	}
}

// upToDate must appear as true, or a script cannot tell "nothing to do" from a
// missing field.
func TestUpdateJSONOnTheNewestRelease(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testLatestVersion)

	stdout, stderr, err := runUpdateCmd(t, "update", "--json")
	if err != nil {
		t.Fatalf("update --json on the newest release: %v", err)
	}
	assertJSONFields(t, decodeJSON(t, stdout), map[string]any{
		"current":  testLatestVersion,
		"latest":   testLatestVersion,
		"upToDate": true,
		"method":   "swap",
		"path":     binary,
	})
	if strings.Contains(stderr, "no release newer than") {
		t.Errorf("stderr = %q, want the narration suppressed under --json", stderr)
	}
	if n := mirror.assetRequests(); n != 0 {
		t.Errorf("an up-to-date binary made %d asset requests, want 0", n)
	}
}

func TestUpdateRefusesADevelopmentBuild(t *testing.T) {
	dir := t.TempDir()
	installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, "dev")

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update ran on a development build")
	}
	if !strings.Contains(err.Error(), "development build") {
		t.Errorf("error = %q, want it to name the development build", err)
	}
	if n := mirror.requestCount(); n != 0 {
		t.Errorf("a development build made %d requests to the mirror, want 0", n)
	}
}

// Two entries claiming `ronja` are ambiguous, not a race the updater resolves by
// picking one.
func TestUpdateRefusesAmbiguousArchiveShapes(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"a symlink wearing the name", []tarEntry{
			{name: "ronja", typeflag: tar.TypeSymlink, linkname: "/usr/bin/env"},
		}},
		{"a hard link wearing the name", []tarEntry{
			{name: "README.md", body: []byte("# ronja\n")},
			{name: "ronja", typeflag: tar.TypeLink, linkname: "README.md"},
		}},
		{"two entries claiming the name", []tarEntry{
			{name: "ronja", body: []byte(newBinaryBytes)},
			{name: "ronja", body: []byte("something else entirely\n")},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := isolateTempDir(t)
			dir := t.TempDir()
			binary := installedBinary(t, dir)
			mirror := newFakeMirror(t, testLatestVersion, tarballOf(t, tc.entries...))
			useMirror(t, mirror, testCurrentVersion)

			_, _, err := runUpdateCmd(t, "update")
			if err == nil {
				t.Fatal("update accepted the archive")
			}
			if body := readFile(t, binary); body != oldBinaryBytes {
				t.Errorf("binary = %q, want the original bytes untouched", body)
			}
			assertNoResidue(t, dir)
			assertNoArchiveResidue(t, tmp)
		})
	}
}

func TestUpdateLeavesAFreshForeignProbeAlone(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	live := filepath.Join(dir, ".ronja-update-999")
	if err := os.WriteFile(live, []byte("a concurrent run's probe"), 0o600); err != nil {
		t.Fatalf("plant a live probe: %v", err)
	}

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	if _, _, err := runUpdateCmd(t, "update"); err != nil {
		t.Fatalf("update: %v", err)
	}
	assertBinaryReplaced(t, binary)
	if body := readFile(t, live); body != "a concurrent run's probe" {
		t.Errorf("the live probe was swept: %q", body)
	}
}

// Refused rather than adopted, which would let two runs write the same file.
func TestUpdateRefusesAProbeItDidNotCreate(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	probe := filepath.Join(dir, fmt.Sprintf(".ronja-update-%d", os.Getpid()))
	if err := os.WriteFile(probe, []byte("another run's probe"), 0o600); err != nil {
		t.Fatalf("plant a colliding probe: %v", err)
	}

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update wrote into a probe it did not create")
	}
	if !strings.Contains(err.Error(), probe) || !strings.Contains(err.Error(), "delete it") {
		t.Errorf("error = %q, want it to name %s and say when it can be deleted", err, probe)
	}
	if n := mirror.assetRequests(); n != 0 {
		t.Errorf("the refusal made %d asset requests, want 0", n)
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("binary = %q, want the original bytes untouched", body)
	}
	if body := readFile(t, probe); body != "another run's probe" {
		t.Errorf("the refusal touched the file it named: %q", body)
	}
}

func TestUpdateKeepsItsProbeAliveThroughALongDownload(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	probe := filepath.Join(dir, fmt.Sprintf(".ronja-update-%d", os.Getpid()))

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	var aged, survived atomic.Bool
	mirror.hook = func(path string) {
		switch {
		case strings.HasSuffix(path, update.ChecksumsName):
			old := time.Now().Add(-2 * staleProbeAge)
			if err := os.Chtimes(probe, old, old); err == nil {
				aged.Store(true)
			}
		case strings.HasSuffix(path, ".tar.gz"):
			sweepStaleProbes(dir)
			if _, err := os.Stat(probe); err == nil {
				survived.Store(true)
			}
		}
	}
	useMirror(t, mirror, testCurrentVersion)

	if _, _, err := runUpdateCmd(t, "update"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !aged.Load() {
		t.Fatal("the probe was never aged, so the sweep below proves nothing")
	}
	if !survived.Load() {
		t.Error("a concurrent sweep removed this run's live probe")
	}
	assertBinaryReplaced(t, binary)
	assertNoResidue(t, dir)
}

func TestUpdateCheckKeepsNotifiedAt(t *testing.T) {
	dir := t.TempDir()
	installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	notified := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	if err := update.SaveState(update.State{NotifiedAt: notified, Latest: testCurrentVersion}); err != nil {
		t.Fatalf("seed the state file: %v", err)
	}

	if _, _, err := runUpdateCmd(t, "update", "--check"); err != nil {
		t.Fatalf("update --check: %v", err)
	}
	state, err := update.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !state.NotifiedAt.Equal(notified) {
		t.Errorf("NotifiedAt = %s, want the seeded %s to survive", state.NotifiedAt, notified)
	}
	if state.Latest != testLatestVersion {
		t.Errorf("state Latest = %q, want %q", state.Latest, testLatestVersion)
	}
	if state.CheckedAt.IsZero() {
		t.Error("state CheckedAt was not recorded")
	}
}

func TestUpdateLeavesAnUnreadableStateAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 file")
	}
	dir := t.TempDir()
	installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	notified := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	if err := update.SaveState(update.State{NotifiedAt: notified, Latest: testCurrentVersion}); err != nil {
		t.Fatalf("seed the state file: %v", err)
	}
	path := filepath.Join(os.Getenv("RONJA_CONFIG_DIR"), update.StateFileName)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}

	if _, _, err := runUpdateCmd(t, "update", "--check"); err != nil {
		t.Fatalf("update --check: %v", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod %s back: %v", path, err)
	}
	state, err := update.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !state.NotifiedAt.Equal(notified) {
		t.Errorf("NotifiedAt = %s, want the seeded %s — an unreadable cache was written over", state.NotifiedAt, notified)
	}
}

func TestUpdateRefusesAnIncompleteRelease(t *testing.T) {
	asset := update.AssetName(testLatestVersion, runtime.GOOS, runtime.GOARCH)
	for _, missing := range []string{asset, update.ChecksumsName} {
		t.Run("without "+missing, func(t *testing.T) {
			dir := t.TempDir()
			binary := installedBinary(t, dir)
			mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
			delete(mirror.assets, missing)
			useMirror(t, mirror, testCurrentVersion)

			_, _, err := runUpdateCmd(t, "update")
			if err == nil {
				t.Fatalf("update accepted a release with no %s", missing)
			}
			if !strings.Contains(err.Error(), "has no asset for") {
				t.Errorf("error = %q, want it to name the missing asset", err)
			}
			if n := mirror.assetRequests(); n != 0 {
				t.Errorf("the refusal made %d asset requests, want 0", n)
			}
			if body := readFile(t, binary); body != oldBinaryBytes {
				t.Errorf("binary = %q, want the original bytes untouched", body)
			}
			assertNoResidue(t, dir)
		})
	}
}

func TestUpdateJSONOnTheSwapPath(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	stdout, stderr, err := runUpdateCmd(t, "update", "--json")
	if err != nil {
		t.Fatalf("update --json: %v", err)
	}
	payload := decodeJSON(t, stdout)
	want := map[string]any{
		"current":  testCurrentVersion,
		"latest":   testLatestVersion,
		"upToDate": false,
		"method":   "swap",
		"path":     binary,
		"asset":    update.AssetName(testLatestVersion, runtime.GOOS, runtime.GOARCH),
		"updated":  true,
	}
	assertJSONFields(t, payload, want)
	if strings.Contains(stderr, "Updated ronja") {
		t.Errorf("stderr = %q, want the narration suppressed under --json", stderr)
	}
	assertBinaryReplaced(t, binary)
}

func TestUpdateJSONOnTheHomebrewPath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Caskroom", "ronja", "0.28.4")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stage cask dir: %v", err)
	}
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	stdout, stderr, err := runUpdateCmd(t, "update", "--json")
	if err != nil {
		t.Fatalf("update --json on a Homebrew install: %v", err)
	}
	payload := decodeJSON(t, stdout)
	assertJSONFields(t, payload, map[string]any{
		"current": testCurrentVersion,
		"method":  "homebrew",
		"command": "brew upgrade ronja",
		"path":    binary,
	})
	if strings.Contains(stderr, "brew upgrade") {
		t.Errorf("stderr = %q, want the narration suppressed under --json", stderr)
	}
	if n := mirror.requestCount(); n != 0 {
		t.Errorf("the Homebrew path made %d requests to the mirror, want 0", n)
	}
}

func TestUpdateSurfacesAReleaseLookupFailure(t *testing.T) {
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	mirror.failRelease = true
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update succeeded against a mirror answering 500")
	}
	if !strings.Contains(err.Error(), "could not read the latest release") {
		t.Errorf("error = %q, want it to say the latest release could not be read", err)
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("binary = %q, want the original bytes untouched", body)
	}
	assertNoResidue(t, dir)
}

func TestUpdateRefusesAnUnparseableTag(t *testing.T) {
	dir := t.TempDir()
	installedBinary(t, dir)
	mirror := newFakeMirror(t, "nightly", releaseTarball(t))
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update accepted a release tagged 'nightly'")
	}
	if !strings.Contains(err.Error(), `tagged "nightly"`) || !strings.Contains(err.Error(), "not a release version") {
		t.Errorf("error = %q, want it to name the tag and say it is not a release version", err)
	}
	if strings.Contains(err.Error(), "could not read") {
		t.Errorf("error = %q — the release WAS read; the message must not say otherwise", err)
	}
}

func TestUpdateRefusesATruncatedDownload(t *testing.T) {
	tmp := isolateTempDir(t)
	dir := t.TempDir()
	binary := installedBinary(t, dir)
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	mirror.truncate = true
	useMirror(t, mirror, testCurrentVersion)

	_, _, err := runUpdateCmd(t, "update")
	if err == nil {
		t.Fatal("update accepted a truncated download")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %q, want a checksum mismatch", err)
	}
	if body := readFile(t, binary); body != oldBinaryBytes {
		t.Errorf("binary = %q, want the original bytes untouched", body)
	}
	assertNoResidue(t, dir)
	assertNoArchiveResidue(t, tmp)
}

func assertNoResidue(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read binary dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "ronja" {
			t.Errorf("a failed update left %q in the binary's directory", e.Name())
		}
	}
}

func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func assertNoArchiveResidue(t *testing.T, dir string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, "ronja-update-*"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(left) > 0 {
		t.Errorf("a failed update left %v in the temporary directory", left)
	}
}

// assertJSONFields fails on an extra field too: a field that appears on one
// path and not another is the bug this pins.
func assertJSONFields(t *testing.T, payload, want map[string]any) {
	t.Helper()
	for field, value := range want {
		if payload[field] != value {
			t.Errorf("json %s = %v, want %v", field, payload[field], value)
		}
	}
	for field := range payload {
		if _, expected := want[field]; !expected {
			t.Errorf("json carries an unexpected field %q = %v", field, payload[field])
		}
	}
}
