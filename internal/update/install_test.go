package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledByHomebrew(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/opt/homebrew/Caskroom/ronja/0.28.4/ronja", true},
		{"/usr/local/Caskroom/ronja/0.28.4/ronja", true},
		// The case a $HOMEBREW_PREFIX test would get wrong: on an Intel Mac
		// the prefix IS /usr/local, and /usr/local/bin is where somebody who
		// unpacked the release tarball puts the binary. Telling them to run
		// `brew upgrade ronja` would send them to a cask that is not installed.
		{"/usr/local/bin/ronja", false},
		{"/home/adam/go/bin/ronja", false},
		{"/usr/bin/ronja", false},
		// A directory merely NAMED like the marker is not the marker.
		{"/home/adam/CaskroomNotes/ronja", false},
	}
	for _, tc := range cases {
		if got := InstalledByHomebrew(tc.path); got != tc.want {
			t.Errorf("InstalledByHomebrew(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The path a user actually runs is the symlink in <prefix>/bin, so resolving it
// is what makes the Caskroom marker reachable at all.
func TestInstalledByHomebrewFollowsSymlink(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "Caskroom", "ronja", "0.29.0")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatalf("stage cask dir: %v", err)
	}
	real := filepath.Join(staged, "ronja")
	if err := os.WriteFile(real, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write staged binary: %v", err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("make bin dir: %v", err)
	}
	link := filepath.Join(bin, "ronja")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if !InstalledByHomebrew(link) {
		t.Errorf("InstalledByHomebrew(%q) = false, want true through the symlink", link)
	}
}

func TestAssetName(t *testing.T) {
	// The `v` is dropped: goreleaser's archive template uses .Version while
	// the version stamp uses .Tag.
	if got := AssetName("v0.29.0", "darwin", "arm64"); got != "ronja_0.29.0_darwin_arm64.tar.gz" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.29.0", "linux", "amd64"); got != "ronja_0.29.0_linux_amd64.tar.gz" {
		t.Errorf("AssetName without a v = %q", got)
	}
}

// digest is a stand-in SHA-256: the LENGTH is part of what makes a line a
// checksum line, so a four-character marker would now be skipped as not one.
func digest(c string) string { return strings.Repeat(c, sha256HexLen) }

func TestChecksumFor(t *testing.T) {
	manifest := []byte("" +
		digest("a") + "  ronja_0.29.0_darwin_amd64.tar.gz\n" +
		digest("b") + "  ronja_0.29.0_darwin_arm64.tar.gz\n" +
		digest("c") + " *ronja_0.29.0_linux_amd64.tar.gz\n" +
		"\n")
	if sum, err := ChecksumFor(manifest, "ronja_0.29.0_darwin_arm64.tar.gz"); err != nil || sum != digest("b") {
		t.Errorf("ChecksumFor = (%q, %v), want (%s, nil)", sum, err, digest("b"))
	}
	// The binary-mode `*name` form parses too, so a manifest from another
	// producer is not read as "this asset is not listed".
	if sum, err := ChecksumFor(manifest, "ronja_0.29.0_linux_amd64.tar.gz"); err != nil || sum != digest("c") {
		t.Errorf("ChecksumFor(binary form) = (%q, %v), want (%s, nil)", sum, err, digest("c"))
	}
	if _, err := ChecksumFor(manifest, "ronja_0.29.0_linux_arm64.tar.gz"); !errors.Is(err, ErrChecksumNotListed) {
		t.Errorf("ChecksumFor for an absent asset = %v, want ErrChecksumNotListed", err)
	}
}

// The digest field is validated, so a line that is not a checksum line cannot
// be returned as one. Each shape here has an asset name in it somewhere and a
// first field that is not a 64-character hex digest; answering with that field
// would hand the caller something the archive can only mismatch, and the user
// would read a corrupt-download refusal where the truth is "this manifest does
// not list your platform".
func TestChecksumForSkipsLinesThatAreNotDigests(t *testing.T) {
	asset := "ronja_0.29.0_darwin_arm64.tar.gz"
	cases := map[string]string{
		// GNU coreutils' own comment form, and the shape a human adds.
		"a comment line": "# checksums for " + asset + "\n",
		// A header some pipelines print above the list.
		"a header line": "SHA256 " + asset + "\n",
		// BSD's `shasum --tag` output: the name is in parentheses and the
		// digest is LAST, so the first field is the algorithm.
		"the BSD tag form": "SHA256 (" + asset + ") = " + digest("a") + "\n",
		// A UTF-8 BOM on the first line, which is what a Windows editor
		// leaves behind: the digest is there but the field no longer is one.
		"a BOM before the digest": "\ufeff" + digest("a") + "  " + asset + "\n",
		// Hex, but half a digest.
		"a truncated digest": strings.Repeat("a", 32) + "  " + asset + "\n",
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			sum, err := ChecksumFor([]byte(manifest), asset)
			if !errors.Is(err, ErrChecksumNotListed) {
				t.Fatalf("ChecksumFor = (%q, %v), want ErrChecksumNotListed", sum, err)
			}
		})
	}
}

// A BOM on the first line must not hide the digest on the SECOND one: the
// refusal above is about the line the BOM is on, not about the manifest.
func TestChecksumForReadsPastABOMLine(t *testing.T) {
	asset := "ronja_0.29.0_darwin_arm64.tar.gz"
	manifest := []byte("\ufeff# ronja checksums\n" + digest("a") + "  " + asset + "\n")
	if sum, err := ChecksumFor(manifest, asset); err != nil || sum != digest("a") {
		t.Errorf("ChecksumFor = (%q, %v), want the digest on the second line", sum, err)
	}
}

// Hex is a number: a producer that writes the digest in upper case has not
// said anything different, and the validation must not turn that into "not
// listed".
func TestChecksumForAcceptsUppercaseHex(t *testing.T) {
	asset := "ronja_0.29.0_darwin_arm64.tar.gz"
	upper := strings.ToUpper(digest("a"))
	sum, err := ChecksumFor([]byte(upper+"  "+asset+"\n"), asset)
	if err != nil {
		t.Fatalf("ChecksumFor(uppercase) = %v", err)
	}
	if !strings.EqualFold(sum, digest("a")) {
		t.Errorf("ChecksumFor(uppercase) = %q, want the same digest", sum)
	}
}

// One asset listed twice with two different sums has no right answer: taking
// the first (or the last) would let a manifest carrying both the honest sum and
// somebody else's decide by ordering alone which one the archive is verified
// against. A duplicated line that agrees with itself is not that case, and hex
// is a number rather than a string — the same digest in upper case is the same
// digest.
func TestChecksumForDuplicates(t *testing.T) {
	asset := "ronja_0.29.0_darwin_arm64.tar.gz"

	same := []byte(digest("a") + "  " + asset + "\n" + digest("a") + "  " + asset + "\n")
	if sum, err := ChecksumFor(same, asset); err != nil || sum != digest("a") {
		t.Errorf("ChecksumFor(duplicate, same sum) = (%q, %v), want (%s, nil)", sum, err, digest("a"))
	}

	cased := []byte(strings.Repeat("aA", sha256HexLen/2) + "  " + asset + "\n" + digest("a") + "  " + asset + "\n")
	if _, err := ChecksumFor(cased, asset); err != nil {
		t.Errorf("ChecksumFor(duplicate, same sum in another case) = %v, want nil", err)
	}

	conflicting := []byte(digest("a") + "  " + asset + "\n" + digest("b") + "  " + asset + "\n")
	if _, err := ChecksumFor(conflicting, asset); !errors.Is(err, ErrChecksumConflict) {
		t.Errorf("ChecksumFor(conflicting sums) = %v, want ErrChecksumConflict", err)
	}
}

// The name is the whole remainder of the line, not the second whitespace-
// separated field. goreleaser never emits a name with a space in it, so this is
// not a case that has to work — but splitting on whitespace would have made
// such a name silently unfindable, which reads to a user as "this release has
// no checksum for your platform".
func TestChecksumForANameWithASpace(t *testing.T) {
	manifest := []byte(digest("d") + "  ronja 0.29.0 darwin arm64.tar.gz\n")
	if sum, err := ChecksumFor(manifest, "ronja 0.29.0 darwin arm64.tar.gz"); err != nil || sum != digest("d") {
		t.Errorf("ChecksumFor = (%q, %v), want (%s, nil)", sum, err, digest("d"))
	}
}

func TestNotice(t *testing.T) {
	cases := map[Method]string{
		MethodSwap:      "ronja v0.29.0 is available (you have v0.28.4) — run: ronja update",
		MethodHomebrew:  "ronja v0.29.0 is available (you have v0.28.4) — run: brew upgrade ronja",
		MethodGoInstall: "ronja v0.29.0 is available (you have v0.28.4) — run: go install github.com/ronjatech/ronja-cli/cmd/ronja@latest",
	}
	for method, want := range cases {
		if got := Notice("v0.28.4", "v0.29.0", method); got != want {
			t.Errorf("Notice(%s) = %q, want %q", method, got, want)
		}
	}
}
