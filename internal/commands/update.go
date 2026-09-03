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

// `ronja update` replaces the running binary with the newest published release.
//
// It is the one command in the tree that touches no Ronja resource and carries
// no Ronja credential — housekeeping of the binary itself. That is why it is
// admitted without extending the "sync verbs yes, resource verbs no" doctrine
// rather than in spite of it: it is not a verb over anything on the server.
//
// It never runs implicitly. A CLI that rewrites itself as a side effect of a
// command someone asked for is a CLI whose bytes change under a CI job that
// pinned a version, so the swap only ever happens because somebody typed this.
//
// The trust boundary is stated in --help and in cli/README.md, and it has a
// hole in it: the checksum manifest comes from the same origin as the archive
// it vouches for, so whoever can publish one can publish the other. What is
// verified here is integrity, not provenance. The interim narrowing is the
// host rule in internal/update — the download cannot leave GitHub's own hosts,
// on the first request or on any redirect — and the close is a signature.
// See BL-6d3a.

// releaseBaseURL is the GitHub API host the release lookup runs against.
//
// A package var, and the ONLY seam: there is deliberately no environment
// variable for it, because it decides which bytes get written over the running
// executable. An env var would make that a per-invocation choice anything on
// the machine could make.
var releaseBaseURL = update.DefaultBaseURL

// currentExecutable resolves the running binary. A var for the same reason
// isTerminal is one: the swap path cannot be tested against the test binary
// itself, so tests point it at a temp file that stands in for an installed
// ronja.
var currentExecutable = os.Executable

// updateTimeout bounds the RELEASE LOOKUP only — the one small JSON request
// that answers "what is the newest release". Generous compared with the daily
// check's budget, because this one was asked for explicitly and a person is
// waiting for an answer rather than for a command they already ran.
//
// It deliberately does not reach the checksum manifest or the archive download.
// A wall-clock cap on a multi-megabyte transfer is a bandwidth requirement, and
// would refuse to update anyone on a slow link. Those two requests run on the
// command's own context (so Ctrl-C ends them) plus the two bounds that belong
// on a transfer rather than on a clock: the client's ResponseHeaderTimeout for
// a server that never answers, and update.Download's idle-read watchdog for one
// that answers and then goes quiet.
const updateTimeout = 15 * time.Second

// staleProbeAge is how old a leftover .ronja-update-* file must be before the
// sweep removes it.
//
// The age test is the whole point: a bare glob-and-delete would remove the LIVE
// probe of a second `ronja update` running at the same moment, and that run
// would then fail on its own probe with "no such file or directory" — a message
// with nothing in it to connect back to the other process. An hour is far
// longer than an update takes and far shorter than the time anyone would notice
// a stray dotfile.
const staleProbeAge = time.Hour

// probePrefix names the writability probe. One constant because two places
// depend on it agreeing: the run that CREATES the file, and the sweep that
// deletes other runs' leftovers by prefix.
const probePrefix = ".ronja-update-"

// updateReport is the --json object, one type for all four paths this command
// can take.
//
// One struct rather than a map literal per path because the field set is the
// contract a script reads, and four literals are four places for it to drift —
// a key spelled differently on the swap path than on --check is exactly the
// kind of thing that only shows up in somebody's pipeline. The paths differ in
// WHICH fields are set, which is what omitempty expresses:
//
//   - homebrew: current, method, command, path. No latest and no upToDate,
//     because the command answers from the binary's own location and never
//     asks the mirror — there is no newest version it could honestly report.
//   - up to date: current, latest, upToDate=true, method, path.
//   - --check, and a completed swap: the same plus asset; the swap adds
//     updated=true, which is the one field that says something was written.
//
// UpToDate is a POINTER because false is a real answer here: on --check and on
// a swap it has to appear as false rather than be dropped by omitempty.
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
	// The daily notice stays quiet for the rest of this process. The check's
	// own command-name test already covers this command; this is the second
	// belt, because the one thing the notice must never do is announce an
	// available release directly under the line saying it was just installed.
	updateRanThisProcess = true

	current := resolveVersion()
	if !update.IsRelease(current) {
		return fmt.Errorf("ronja is a development build (%s) — there is no release to update to; build from source or install a release", current)
	}
	if runtime.GOOS == "windows" {
		// The release pipeline builds darwin and linux only, so there is no
		// asset to fetch; and renaming over a running executable is not a
		// thing Windows does without a sidecar dance nobody has written.
		return fmt.Errorf("ronja update does not support Windows yet — download the release from %s", update.ReleasesPage())
	}

	exe, err := currentExecutable()
	if err != nil {
		return fmt.Errorf("locate the running ronja binary: %w", err)
	}
	// Resolve the link a user actually runs down to the file that gets
	// replaced. A failure here is not fatal: an unresolvable path is still a
	// path, and the write attempt below is the real test.
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
		// Not "could not read the latest release": it was read, perfectly. The
		// mirror's newest release simply carries a tag that is not a version,
		// and saying so is what tells the reader the problem is over there.
		return fmt.Errorf("the latest release on github.com/%s is tagged %q, which is not a release version — nothing was changed",
			update.Repo, release.Version)
	}

	// Recorded whatever the verdict is, so the daily notice and this command
	// never disagree about what the newest release is. Loaded first and then
	// amended, because the state file is shared with the daily notice: writing
	// a fresh struct would zero NotifiedAt, and the notice would then treat a
	// user who has already been told as one who never has.
	//
	// Best effort throughout: a cache that could not be read or written is not
	// a reason to refuse an update. A cache that could not be READ is not
	// written either — the read is what carries NotifiedAt forward, so writing
	// after a failed one puts a zeroed NotifiedAt on disk and the notice then
	// treats a user it told this morning as one it has never told.
	if state, err := update.LoadState(); err == nil {
		// updateCheckNow, not time.Now: the daily notice's 24-hour arithmetic
		// reads this timestamp, and its tests move that clock. A bare time.Now
		// here would write a real one into a file the rest of the test then
		// reasons about with a fake one.
		state.CheckedAt = updateCheckNow()
		state.Latest = release.Version
		_ = update.SaveState(state)
	}

	if !newer {
		// Worded as a comparison rather than "you are on the latest release",
		// which would be a lie to someone running v0.29.0-rc.1 while the
		// newest stable is v0.28.4.
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
		// release.Version reaches this line UNQUOTED on purpose. It is the
		// mirror's word, but nothing gets here until update.Compare answered
		// ok, and that parse is a charset gate as much as a version one — see
		// update.validPrerelease, which exists precisely so a tag cannot carry
		// a control sequence onto a terminal. The asset name is built from that
		// same version. The strings that are NOT gated this way are the URLs
		// out of the release listing, and update.Safe covers those where they
		// are printed.
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

// swapBinary is the part that changes something on disk, kept in one function
// so the ORDER is readable: probe for writability, then verify, then extract,
// then one rename.
//
// The order is the design. The probe comes before the download so a user on an
// unwritable /usr/local/bin is told so immediately rather than after eight
// megabytes; the archive is verified in os.TempDir() so unverified bytes never
// land beside the binary; and the only thing that ever enters the binary's own
// directory is the extracted, checksum-verified executable, arriving through a
// single same-filesystem rename.
func swapBinary(ctx context.Context, base, exe, asset, assetURL, sumsURL string) error {
	dir := filepath.Dir(exe)

	sweepStaleProbes(dir)

	// O_EXCL, so this never adopts a file somebody else is using: with the
	// sweep leaving fresh probes alone, a collision means a live run.
	probe := filepath.Join(dir, fmt.Sprintf("%s%d", probePrefix, os.Getpid()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return probeRefusal(err, dir, probe)
	}
	f.Close()
	// A no-op after the rename below succeeds, and the cleanup for every
	// failure between here and there.
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

	// Download picks its own unpredictable name under os.TempDir() and hands
	// back the OPEN FILE. Nothing here ever names the archive: the bytes that
	// get extracted below are reached through the same descriptor that was
	// hashed, so there is no window in which the verified file could be
	// swapped for another one. See update.Download.
	archive, got, err := update.Download(ctx, base, assetURL, update.MaxAsset)
	if err != nil {
		return fmt.Errorf("could not download %s: %w", asset, err)
	}
	defer func() {
		archive.Close()
		os.Remove(archive.Name())
	}()
	touchProbe(probe)
	// Case-insensitively: the digest is a number, and a manifest that spells it
	// in upper case has not said anything different.
	if !strings.EqualFold(got, expected) {
		// Every string on this line is safe to print verbatim, and each for its
		// own reason: expected is a validated 64-character digest (update.
		// ChecksumFor will not answer anything else), got is hex.
		// EncodeToString's own output, and asset was built from a version that
		// passed update.Compare's charset gate.
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s) — nothing was changed; the download may be corrupt or tampered with, try again",
			asset, expected, got)
	}

	// Inherit the mode of the binary being replaced, masked to the permission
	// bits — a 0755 stays 0755, but a setuid or setgid bit somebody once put
	// on this file is NOT copied onto freshly downloaded code.
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

// sweepStaleProbes removes the probes of runs that are not running any more.
//
// A SIGKILLed earlier run leaves its probe behind (SIGINT does not — the root
// context's signal handler lets the defers run), so they are swept before a new
// one is created rather than accumulating in the binary's directory. Only files
// old enough to be leftovers: see staleProbeAge for why a bare glob-and-delete
// would sabotage a concurrent run, and touchProbe for how a run on a slow link
// keeps its own probe out of this.
// A directory listing rather than filepath.Glob, because dir is not a pattern:
// it is wherever the binary happens to live, and a `[` or a `*` anywhere in
// that path is a metacharacter to Glob. A user whose binary sits under
// ~/bin[old]/ would get either no match at all or, worse, a match somewhere
// else — and the sweep DELETES what it matches.
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

// touchProbe keeps this run's probe looking alive.
//
// The probe is created once and then not written again until the archive is
// extracted into it, so on a slow link its mtime can age past staleProbeAge
// while the download is still running — and a second `ronja update` starting in
// that hour would sweep a file this run is about to write. Touching it at each
// step it completes says what its age is supposed to say: this run is still
// going.
//
// Best effort. A touch that fails changes nothing about the update, and
// update.ExtractBinary opens the probe with O_CREATE, so even one that WAS
// swept is recreated rather than ending an eight-megabyte download at the last
// step.
func touchProbe(probe string) {
	now := time.Now()
	_ = os.Chtimes(probe, now, now)
}

// probeRefusal turns the errno from creating the probe into a sentence someone
// can act on.
//
// Permission is the common case, but it is not the only one that means "this
// directory is not yours to write": a read-only mount (a container image layer,
// a /usr mounted ro, a macOS sealed system volume) fails with EROFS, and that
// used to fall through to a bare "prepare /usr/bin/.ronja-update-4711: ..."
// naming a dotfile the reader has never heard of and offering no remedy.
//
// EEXIST is a different sentence entirely, because the remedy is. The name
// carries this process's pid and the sweep leaves fresh files alone, so the
// only ways to collide are another `ronja update` running right now under a
// reused pid, or a leftover younger than staleProbeAge — and the reader can
// only tell those apart by looking, so the message names the file and says
// what makes it safe to delete.
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
