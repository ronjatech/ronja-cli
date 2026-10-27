package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ronjatech/ronja-cli/internal/update"
	"github.com/spf13/cobra"
)

// `ronja update` never runs implicitly: a self-rewrite as a side effect would
// change bytes under a CI job that pinned a version. ⚠️ The checksum manifest
// comes from the same origin as the archive, so this verifies integrity, not
// provenance; internal/update's host rule keeps the download on GitHub's hosts,
// redirects included.
// See BL-6d3a.

// releaseBaseURL is deliberately not settable by environment variable: it
// decides which bytes get written over the running executable.
var releaseBaseURL = update.DefaultBaseURL

// currentExecutable is a var so tests can point the swap at a temp file
// instead of the test binary.
var currentExecutable = os.Executable

// hostOS is a var so a test on any OS can take the Windows branches.
var hostOS = runtime.GOOS

// updateTimeout bounds the release lookup only. A wall-clock cap on the
// download would be a bandwidth requirement; transfers rely on the client's
// ResponseHeaderTimeout and update.Download's idle-read watchdog instead.
const updateTimeout = 15 * time.Second

// staleProbeAge keeps the sweep off the live probe of a concurrent
// `ronja update`. An hour, refreshed by touchProbe after each step: far shorter
// than anyone notices a stray dotfile.
const staleProbeAge = time.Hour

// probePrefix must agree between the run that creates the probe and the sweep
// that deletes leftovers by prefix.
const probePrefix = ".ronja-update-"

// updateReport is one type for every path so the field set a script reads
// cannot drift. UpToDate is a pointer because false must appear, not be dropped
// by omitempty.
type updateReport struct {
	Current  string `json:"current"`
	Latest   string `json:"latest,omitempty"`
	UpToDate *bool  `json:"upToDate,omitempty"`
	Method   string `json:"method"`
	Command  string `json:"command,omitempty"`
	Path     string `json:"path"`
	Asset    string `json:"asset,omitempty"`
	Updated  bool   `json:"updated,omitempty"`
}

func newUpdateCmd() *cobra.Command {
	var checkOnly bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update ronja itself to the newest release",
		Long: `Update ronja itself to the newest release.

Downloads the release archive for this platform from
github.com/ronjatech/ronja-cli, verifies it against the published checksum
manifest, and replaces the running binary in place.

  ronja update           install the newest release
  ronja update --check   report what it would do, and change nothing

This never runs on its own — nothing updates ronja except this command.

If ronja was installed with Homebrew, the file belongs to Homebrew and is not
replaced: the command says to run 'brew upgrade ronja' instead, and exits 0.

The checksum is verified against the manifest published with the release. The
checksum is not a signature: it proves the bytes match what the release
published, not who published them — the archive and the manifest come from the
same origin, and both are fetched over https from github.com or not at all.

With --json the report is a single object on stdout. It always carries
'current', 'method' and 'path'. 'latest' and 'upToDate' are present only when
ronja can do the swap itself: on a Homebrew install the command answers from
the binary's own location and asks the mirror nothing, so there is no latest
version to report — it reports 'command' instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd.Context(), checkOnly)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false,
		"report what an update would do, and change nothing")
	return cmd
}

func runUpdate(ctx context.Context, checkOnly bool) error {
	updateRanThisProcess = true

	current := resolveVersion()
	if !update.IsRelease(current) {
		return fmt.Errorf("ronja is a development build (%s) — there is no release to update to; build from source or install a release", current)
	}
	if hostOS == "windows" {
		// Windows refuses a rename over a running executable, which is the
		// whole swap below.
		const refusal = "ronja update cannot replace a running ronja.exe on Windows"
		if updateInstallMethod() == update.MethodGoInstall {
			return fmt.Errorf("%s — run: go install github.com/%s/cmd/ronja@latest", refusal, update.Repo)
		}
		return fmt.Errorf("%s — download ronja_…_windows_%s.zip from %s and replace ronja.exe with the one inside",
			refusal, runtime.GOARCH, update.LatestReleasePage())
	}

	exe, err := currentExecutable()
	if err != nil {
		return fmt.Errorf("locate the running ronja binary: %w", err)
	}
	// Not fatal: the write attempt below is the real test.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	if update.InstalledByHomebrew(exe) {
		if flagJSON {
			return emitJSON(updateReport{
				Current: current,
				Method:  string(update.MethodHomebrew),
				Command: "brew upgrade ronja",
				Path:    exe,
			})
		}
		fmt.Fprintln(os.Stderr, "ronja is installed by Homebrew — run: brew upgrade ronja")
		return nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	release, err := update.Latest(fetchCtx, releaseBaseURL, current)
	if err != nil {
		return fmt.Errorf("could not read the latest release from github.com/%s: %w", update.Repo, err)
	}
	newer, ok := update.Compare(current, release.Version)
	if !ok {
		return fmt.Errorf("the latest release on github.com/%s is tagged %q, which is not a release version — nothing was changed",
			update.Repo, release.Version)
	}

	// Amended, never written fresh and never after a failed read: the file is
	// shared with the daily notice, and a zeroed NotifiedAt re-tells a user.
	// Best effort: a cache failure never refuses an update.
	if state, err := update.LoadState(); err == nil {
		// updateCheckNow, not time.Now: the notice's tests move that clock.
		state.CheckedAt = updateCheckNow()
		state.Latest = release.Version
		_ = update.SaveState(state)
	}

	if !newer {
		// A comparison, not "latest": a v0.29.0-rc.1 is ahead of stable v0.28.4.
		if flagJSON {
			upToDate := true
			return emitJSON(updateReport{
				Current: current, Latest: release.Version, UpToDate: &upToDate,
				Method: string(update.MethodSwap), Path: exe,
			})
		}
		fmt.Fprintf(os.Stderr, "no release newer than ronja %s\n", current)
		return nil
	}

	asset := update.AssetName(release.Version, runtime.GOOS, runtime.GOARCH)
	assetURL, haveAsset := release.Assets[asset]
	sumsURL, haveSums := release.Assets[update.ChecksumsName]
	if !haveAsset || !haveSums {
		return fmt.Errorf("release %s has no asset for %s/%s (or no %s) — see %s",
			release.Version, runtime.GOOS, runtime.GOARCH, update.ChecksumsName,
			update.ReleasePage(release.Version))
	}

	if checkOnly {
		if flagJSON {
			behind := false
			return emitJSON(updateReport{
				Current: current, Latest: release.Version, UpToDate: &behind,
				Method: string(update.MethodSwap), Path: exe, Asset: asset,
			})
		}
		// Unquoted on purpose: update.Compare's parse (validPrerelease) is a
		// charset gate, so the tag and the asset built from it carry no control
		// sequence. Release-listing URLs are not gated; update.Safe covers them.
		fmt.Fprintf(os.Stderr, "ronja %s is available (you have %s). Would replace %s with %s\n",
			release.Version, current, exe, asset)
		return nil
	}

	if err := swapBinary(ctx, releaseBaseURL, exe, asset, assetURL, sumsURL); err != nil {
		return err
	}

	if flagJSON {
		behind := false
		return emitJSON(updateReport{
			Current: current, Latest: release.Version, UpToDate: &behind,
			Method: string(update.MethodSwap), Path: exe, Asset: asset,
			Updated: true,
		})
	}
	fmt.Fprintf(os.Stderr, "Updated ronja %s → %s at %s\n", current, release.Version, exe)
	return nil
}

// swapBinary's order is the design: probe writability before downloading,
// verify the archive in os.TempDir() so unverified bytes never land beside the
// binary, then one same-filesystem rename of the verified executable.
func swapBinary(ctx context.Context, base, exe, asset, assetURL, sumsURL string) error {
	dir := filepath.Dir(exe)

	sweepStaleProbes(dir)

	// O_EXCL: with the sweep leaving fresh probes alone, a collision means a
	// live run.
	probe := filepath.Join(dir, fmt.Sprintf("%s%d", probePrefix, os.Getpid()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return probeRefusal(err, dir, probe)
	}
	f.Close()
	defer os.Remove(probe)

	sums, err := update.Fetch(ctx, base, sumsURL, update.MaxChecksums)
	if err != nil {
		return fmt.Errorf("could not read %s: %w", update.ChecksumsName, err)
	}
	touchProbe(probe)
	expected, err := update.ChecksumFor(sums, asset)
	if err != nil {
		return fmt.Errorf("%s is %w — nothing was changed", asset, err)
	}

	// Extracted through the same open descriptor that was hashed, never by
	// name, so the verified file cannot be swapped in between.
	archive, got, err := update.Download(ctx, base, assetURL, update.MaxAsset)
	if err != nil {
		return fmt.Errorf("could not download %s: %w", asset, err)
	}
	defer func() {
		archive.Close()
		os.Remove(archive.Name())
	}()
	touchProbe(probe)
	if !strings.EqualFold(got, expected) {
		// Each string is safe verbatim: expected is a digest ChecksumFor
		// validated, got is hex output, asset passed Compare's charset gate.
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s) — nothing was changed; the download may be corrupt or tampered with, try again",
			asset, expected, got)
	}

	// Masked to the permission bits: a setuid or setgid bit is never copied
	// onto freshly downloaded code.
	mode := os.FileMode(0o755)
	if info, err := os.Stat(exe); err == nil {
		mode = info.Mode() & 0o777
	}
	if err := update.ExtractBinary(archive, probe, mode); err != nil {
		return fmt.Errorf("%s: %w — nothing was changed", asset, err)
	}

	// Renaming over a running executable is safe on macOS and Linux: the
	// mapped inode outlives the name.
	if err := os.Rename(probe, exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}

// sweepStaleProbes removes probes a SIGKILLed run left behind. A directory
// listing, not filepath.Glob: dir is not a pattern, and a `[` in it would make
// the sweep delete matches somewhere else.
func sweepStaleProbes(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), probePrefix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := os.Stat(p)
		if err != nil || time.Since(info.ModTime()) < staleProbeAge {
			continue
		}
		_ = os.Remove(p)
	}
}

// touchProbe re-stamps the probe after each step swapBinary completes, so a
// slow update's probe stays younger than staleProbeAge between steps. Best
// effort: update.ExtractBinary opens the probe with O_CREATE (download.go:246),
// so one swept during a download longer than staleProbeAge is recreated.
func touchProbe(probe string) {
	now := time.Now()
	_ = os.Chtimes(probe, now, now)
}

// probeRefusal: EEXIST means a concurrent run under a reused pid or a leftover
// younger than staleProbeAge, which only the reader can tell apart.
func probeRefusal(err error, dir, probe string) error {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("cannot write to %s (permission denied) — re-run with sudo, or install the release by the method you used originally (%s)",
			dir, update.ReleasesPage())
	case errors.Is(err, syscall.EROFS):
		return fmt.Errorf("cannot write to %s (read-only file system) — re-run with sudo, or install the release by the method you used originally (%s)",
			dir, update.ReleasesPage())
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%s already exists — another ronja update may be running, or an interrupted one left it behind; delete it if nothing else is updating ronja, then try again",
			probe)
	default:
		return fmt.Errorf("prepare %s: %w", probe, err)
	}
}
