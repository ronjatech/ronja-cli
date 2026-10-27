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
		// The Intel-Mac prefix, where a hand-unpacked tarball also lives.
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
	if got := AssetName("v0.29.0", "darwin", "arm64"); got != "ronja_0.29.0_darwin_arm64.tar.gz" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.29.0", "linux", "amd64"); got != "ronja_0.29.0_linux_amd64.tar.gz" {
		t.Errorf("AssetName without a v = %q", got)
	}
}

// digest is full length because a shorter marker is skipped as not a digest.
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
	if sum, err := ChecksumFor(manifest, "ronja_0.29.0_linux_amd64.tar.gz"); err != nil || sum != digest("c") {
		t.Errorf("ChecksumFor(binary form) = (%q, %v), want (%s, nil)", sum, err, digest("c"))
	}
	if _, err := ChecksumFor(manifest, "ronja_0.29.0_linux_arm64.tar.gz"); !errors.Is(err, ErrChecksumNotListed) {
		t.Errorf("ChecksumFor for an absent asset = %v, want ErrChecksumNotListed", err)
	}
}

// Each shape names the asset but has a first field that is not a digest;
// returning it would read as a corrupt download instead of not listed.
func TestChecksumForSkipsLinesThatAreNotDigests(t *testing.T) {
	asset := "ronja_0.29.0_darwin_arm64.tar.gz"
	cases := map[string]string{
		"a comment line": "# checksums for " + asset + "\n",
		"a header line":  "SHA256 " + asset + "\n",
		// `shasum --tag`: the digest is last, the first field the algorithm.
		"the BSD tag form":        "SHA256 (" + asset + ") = " + digest("a") + "\n",
		"a BOM before the digest": "\ufeff" + digest("a") + "  " + asset + "\n",
		"a truncated digest":      strings.Repeat("a", 32) + "  " + asset + "\n",
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

func TestChecksumForReadsPastABOMLine(t *testing.T) {
	asset := "ronja_0.29.0_darwin_arm64.tar.gz"
	manifest := []byte("\ufeff# ronja checksums\n" + digest("a") + "  " + asset + "\n")
	if sum, err := ChecksumFor(manifest, asset); err != nil || sum != digest("a") {
		t.Errorf("ChecksumFor = (%q, %v), want the digest on the second line", sum, err)
	}
}

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

// goreleaser never emits such a name, but splitting on whitespace would make
// one silently unfindable.
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
		MethodDownload:  "ronja v0.29.0 is available (you have v0.28.4) — download it from https://github.com/ronjatech/ronja-cli/releases/latest",
	}
	for method, want := range cases {
		if got := Notice("v0.28.4", "v0.29.0", method); got != want {
			t.Errorf("Notice(%s) = %q, want %q", method, got, want)
		}
	}
}
