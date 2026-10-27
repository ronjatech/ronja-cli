package update

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Method is how the binary on this machine got here, which decides the remedy
// offered when a newer release exists.
type Method string

const (
	// MethodSwap is a plain file the CLI may replace itself: a release tarball
	// unpacked by hand, or a `go install` build under GOPATH/bin.
	MethodSwap Method = "swap"
	// MethodHomebrew is a cask install; swapping the file would leave brew's
	// manifest describing bytes that are no longer there.
	MethodHomebrew Method = "homebrew"
	// MethodGoInstall is a `go install` build on Windows, where `ronja update`
	// refuses.
	MethodGoInstall Method = "go-install"
	// MethodDownload is a release zip on Windows, where `ronja update` refuses.
	MethodDownload Method = "download"
)

// AssetName is the release archive for one platform. The `v` is dropped
// because the archive name uses goreleaser's `.Version` while the version stamp
// uses `.Tag` (see cli/.goreleaser.yaml).
func AssetName(version, goos, goarch string) string {
	return fmt.Sprintf("ronja_%s_%s_%s.tar.gz", strings.TrimPrefix(version, "v"), goos, goarch)
}

// ErrChecksumNotListed is returned when the manifest says nothing about the
// asset — which is a refusal to update, not a checksum failure.
var ErrChecksumNotListed = errors.New("not listed in the checksum manifest")

// ErrChecksumConflict is returned when the manifest lists one asset twice with
// different sums. Picking either would let line order decide which sum is
// verified against; a duplicated line with the same sum is not a conflict.
var ErrChecksumConflict = errors.New("listed twice in the checksum manifest with different sums")

// ChecksumFor finds one asset's SHA-256 in a goreleaser checksums.txt: lines of
// `<hex><whitespace><name>`, the name being the trimmed remainder (a leading
// `*` binary-mode marker dropped). The whole manifest is read, since a
// conflicting duplicate is only known at the end.
//
// A line whose first field is not a 64-character hex digest (a header, a BOM,
// the BSD `SHA256 (name) = …` form) is skipped, so it reads as not listed
// rather than as a tampered download, and what is returned can never carry a
// control sequence from the manifest into the caller's message.
func ChecksumFor(manifest []byte, asset string) (string, error) {
	found := ""
	for _, line := range strings.Split(string(manifest), "\n") {
		line = strings.TrimSpace(line)
		gap := strings.IndexAny(line, " \t")
		if gap < 0 {
			continue
		}
		sum := line[:gap]
		if !isSHA256Hex(sum) {
			continue
		}
		name := strings.TrimPrefix(strings.TrimSpace(line[gap:]), "*")
		if name != asset {
			continue
		}
		if found != "" && !strings.EqualFold(found, sum) {
			return "", ErrChecksumConflict
		}
		found = sum
	}
	if found == "" {
		return "", ErrChecksumNotListed
	}
	return found, nil
}

const sha256HexLen = 64

func isSHA256Hex(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// InstalledByHomebrew reports whether exe was installed by the Homebrew cask:
// a `Caskroom` element in its symlink-resolved path. Not a $HOMEBREW_PREFIX
// test, because on an Intel Mac that prefix is /usr/local, where a hand-unpacked
// tarball also lives.
func InstalledByHomebrew(exe string) bool {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	for _, element := range strings.Split(filepath.ToSlash(resolved), "/") {
		if element == "Caskroom" {
			return true
		}
	}
	return false
}

// Notice is the one line printed when a newer release exists, with the remedy
// chosen by how this binary was installed.
func Notice(current, latest string, method Method) string {
	remedy := "run: ronja update"
	switch method {
	case MethodHomebrew:
		remedy = "run: brew upgrade ronja"
	case MethodGoInstall:
		remedy = "run: go install github.com/" + Repo + "/cmd/ronja@latest"
	case MethodDownload:
		remedy = "download it from " + LatestReleasePage()
	}
	return fmt.Sprintf("ronja %s is available (you have %s) — %s", latest, current, remedy)
}
