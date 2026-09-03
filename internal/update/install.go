package update

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Method is how the binary on this machine got here, which is what decides the
// remedy offered when a newer release exists.
type Method string

const (
	// MethodSwap is a plain file the CLI may replace itself: a release tarball
	// unpacked by hand, or a `go install` build under GOPATH/bin.
	MethodSwap Method = "swap"
	// MethodHomebrew is a cask install. Homebrew owns the file; swapping it
	// would leave brew's manifest describing bytes that are no longer there.
	MethodHomebrew Method = "homebrew"
	// MethodGoInstall is the remedy on a platform with no release asset —
	// Windows, where the release pipeline builds nothing and `ronja update`
	// refuses, but `go install` still produces a working, stamped binary.
	MethodGoInstall Method = "go-install"
)

// AssetName is the release archive for one platform.
//
// The `v` is dropped because goreleaser's archive name_template uses `.Version`
// (`ronja_0.28.4_darwin_arm64.tar.gz`) while the ldflags stamp uses `.Tag`
// (`v0.28.4`) — see .goreleaser.yaml, where the divergence is deliberate and
// explained.
func AssetName(version, goos, goarch string) string {
	return fmt.Sprintf("ronja_%s_%s_%s.tar.gz", strings.TrimPrefix(version, "v"), goos, goarch)
}

// ErrChecksumNotListed is returned when the manifest says nothing about the
// asset — which is a refusal to update, not a checksum failure.
var ErrChecksumNotListed = errors.New("not listed in the checksum manifest")

// ErrChecksumConflict is returned when the manifest lists one asset twice with
// two DIFFERENT sums.
//
// There is no right answer to pick, so there is nothing to install. Taking the
// first (or the last) would let a manifest carrying both the honest sum and
// somebody else's decide which one the updater verifies against by ordering
// alone. Two lines with the SAME sum are not a conflict — that is a duplicated
// line, and it agrees with itself.
var ErrChecksumConflict = errors.New("listed twice in the checksum manifest with different sums")

// ChecksumFor finds one asset's SHA-256 in a goreleaser checksums.txt.
//
// The format is `<hex>  <name>` with two spaces, but any whitespace run is
// accepted: the single-space and `*name` (binary-mode) forms are what other
// producers of the same file emit, and a checksum that fails to parse would be
// read as "not listed", i.e. as a refusal to update.
//
// The parse is `<hex><whitespace><name-to-end-of-line>`: the hex is the first
// field and the NAME IS THE WHOLE REMAINDER, trimmed. goreleaser never emits an
// asset name with a space in it — the template is
// ronja_{{.Version}}_{{.Os}}_{{.Arch}} — so this is not a case that has to
// work; splitting on every whitespace run would simply have made a name that
// did contain one silently unfindable, which reads to a user as "this release
// has no checksum for your platform".
//
// The whole manifest is read even after a hit, because "listed twice" is only
// answerable at the end. Hex is compared case-insensitively throughout: the
// digest is a number, and a producer that writes it in upper case has not said
// anything different.
//
// A line whose first field is not a SHA-256 digest is not a checksum line at
// all, and is skipped rather than parsed. That is what keeps a header, a
// comment, a UTF-8 BOM on the first line, or the BSD-style
// `SHA256 (name) = <hex>` form from being answered as `expected`: each of those
// has a first field that is not hex, and returning one would hand the caller a
// "checksum" it then compares the archive against — which can only ever
// mismatch, so a manifest with a comment at the top would read to the user as
// a tampered download. Refusing to see it means the asset is simply not listed
// (ErrChecksumNotListed), which is the truth. It also means everything this
// function returns is 64 hex characters, so the mismatch message the caller
// prints cannot carry a control sequence from the manifest.
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

// sha256HexLen is a SHA-256 digest written in hex: 32 bytes, two characters
// each.
const sha256HexLen = 64

// isSHA256Hex reports whether s is exactly a SHA-256 digest in hex.
//
// Hand-written rather than hex.DecodeString because the LENGTH is half the
// test: a shorter run of hex characters decodes perfectly well and is still
// not a digest.
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

// InstalledByHomebrew reports whether exe was installed by the Homebrew cask.
//
// The test is one thing: a `Caskroom` element in the fully resolved path. The
// cask stages the binary at <prefix>/Caskroom/ronja/<version>/ronja and
// symlinks <prefix>/bin/ronja to it, so resolving symlinks is what turns the
// link a user actually runs into the marker.
//
// Deliberately NOT a $HOMEBREW_PREFIX test, and deliberately no `brew`
// subprocess. On an Intel Mac the prefix is /usr/local, and /usr/local/bin is
// exactly where somebody who unpacked the release tarball puts the binary —
// a prefix test would tell them to run `brew upgrade ronja` and brew would
// answer "Cask 'ronja' is not installed". Caskroom is the only marker that
// means what it says.
func InstalledByHomebrew(exe string) bool {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// An unresolvable path is not evidence of a cask install, and guessing
		// "yes" would refuse to update a binary that is perfectly swappable.
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
//
// Versions are rendered exactly as `ronja --version` prints them, leading `v`
// included, so the two can never appear to disagree.
func Notice(current, latest string, method Method) string {
	remedy := "ronja update"
	switch method {
	case MethodHomebrew:
		remedy = "brew upgrade ronja"
	case MethodGoInstall:
		remedy = "go install github.com/" + Repo + "/cmd/ronja@latest"
	}
	return fmt.Sprintf("ronja %s is available (you have %s) — run: %s", latest, current, remedy)
}
