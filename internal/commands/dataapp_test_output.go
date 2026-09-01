package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// What a render leaves on disk: where it goes, the report, the filmstrip, and
// the removal of the previous run's frames before any of it is written.
// Everything here owns --out-dir — where it resolves to, which names it writes,
// which it clears, and which failures are a warning rather than the end of the
// command.

// Names of the files written into --out-dir. Fixed rather than derived from the
// app id or the run: a caller scripting this needs to know where the report is
// without parsing anything, and re-running overwrites rather than accumulating.
const (
	appTestReportName   = "report.json"
	appTestSelectedName = "screenshot.png"
	appTestFramePattern = "screenshot-%d.png"
	appTestOutDirPerm   = 0o700
	// appTestDirName is the subdirectory of the folder's own .ronja/ that
	// --out-dir defaults to.
	//
	// The old default was the folder ROOT, and it put a PNG beside App.tsx: the
	// next `ronja app push` enumerated it, found NUL bytes, and refused the whole
	// folder — one command's output making the next one impossible. Under
	// .ronja/ the walk never sees them at all (wfdir.StructuralExclusion rules
	// the state directory out by name, and does not even report it as skipped,
	// because it is the CLI's own bookkeeping rather than the user's mistake),
	// and the "*" .gitignore already living there keeps them out of git too.
	appTestDirName = "test"
)

// appTestOutDir is the resolved destination, plus the one thing that can go
// wrong on the way there without stopping the run.
type appTestOutDir struct {
	Path string
	// NotIgnored is the folder root whose .ronja/.gitignore could not be
	// written, empty when the ignore is in place or when --out-dir was named
	// (that directory is the caller's own, and its git rules are theirs).
	NotIgnored string
	// IgnoreErr is why, kept so the warning can say it.
	IgnoreErr error
}

// resolveAppTestOutDir decides where this run's artefacts go, and says so.
//
// A named --out-dir is used EXACTLY as given — the app folder itself included.
// The default moved out of the folder root because one command's output was
// making the next command impossible (see appTestDirName), not because writing
// there is wrong, and someone who asks for it gets it.
//
// The default is narrated on stderr rather than left to be discovered. The
// artefacts have not become less findable — the "Wrote:" block lists every path
// in full, and --json carries them too — but a location nobody typed has to be
// stated, and it is the only line here a reader needs to know the answer moved.
func resolveAppTestOutDir(named, root string) (appTestOutDir, error) {
	if named != "" {
		return appTestOutDir{Path: named}, nil
	}
	// The default path is chosen by this command, not the user, and the folder
	// may have come from a git clone — so a committed symlink anywhere along
	// .ronja/test must not silently redirect the writes, or the pre-write
	// cleanup's DELETES, out of the folder. wfdir.StateDir owns that check for
	// every writer under .ronja/, this one included. A named --out-dir is the
	// user's own choice and is used as given, symlink or not.
	dir, err := wfdir.StateDir(root, appTestDirName)
	if err != nil {
		return appTestOutDir{}, fmt.Errorf("%w, or pass --out-dir to choose the destination yourself", err)
	}
	out := appTestOutDir{Path: dir}
	// The state directory carries a "*" .gitignore so nothing inside it can be
	// committed by accident. A folder cloned from git correctly has no .ronja/
	// at all — the baseline is per-user and never committed — so this run may be
	// the first thing to create one, and the ignore is asserted rather than
	// assumed. A failure here loses that protection and nothing else, so it is a
	// warning: refusing would throw away an observation over the way it is
	// stored. It is said twice instead — see WarnIfNotIgnored.
	if err := wfdir.WriteStateGitignore(root); err != nil {
		out.NotIgnored, out.IgnoreErr = root, err
		out.warnNotIgnored("")
	}
	fmt.Fprintf(os.Stderr, "  Writing to %s — the folder's local-only %s/, which `ronja app push` never syncs. Pass --out-dir to put them elsewhere.\n",
		dir, wfdir.StateDirName)
	return out, nil
}

// WarnIfNotIgnored repeats the ignore warning after the report has been
// printed, when there is one.
//
// The warning is about tenant pixels and tenant query results sitting
// un-ignored in somebody's repository, and it is raised BEFORE a render that
// can take twenty seconds and then prints a screenful — so on its own it is a
// line nobody sees. Repeating it costs one line and lands where the reader is,
// beside the paths that were just written.
func (o appTestOutDir) WarnIfNotIgnored() {
	if o.NotIgnored == "" {
		return
	}
	o.warnNotIgnored("(again) ")
}

func (o appTestOutDir) warnNotIgnored(prefix string) {
	fmt.Fprintf(os.Stderr, "  Warning: %scould not write %s (%v), so nothing is ignoring these files: the screenshots and query results in %s are tenant data, and a `git add .` in %s would commit them.\n",
		prefix, filepath.Join(wfdir.StateDirName, wfdir.GitignoreName), o.IgnoreErr, o.Path, o.NotIgnored)
}

// appTestArtifactHint is the extra line a push refusal carries when the files it
// is refusing are this command's own output, or "" when they are not.
//
// It exists for folders that already hold artefacts from a CLI that wrote them
// into the folder root. Those files are NOT ignored by name — a user's own
// report.json must still sync, or refuse loudly, rather than being silently
// dropped by a rule about a name — so the only help available is to say what
// they look like once the refusal has already fired.
//
// Gated on the KIND, because only a data-app folder has the command being
// pointed at. A workflow folder refused for holding screenshot-1.png is holding
// something else's output — `ronja wf` has no `test --out-dir` to move it with,
// and naming one would be the same wrong-primitive answer the kind-aware
// refusal above it exists to stop giving.
func appTestArtifactHint(kind wfdir.Kind, paths []string) string {
	if kind.Name != wfdir.KindDataApp {
		return ""
	}
	for _, path := range paths {
		if isAppTestArtifact(path) {
			return fmt.Sprintf("  Some of these look like `ronja app test` output. It writes into %s now, which is never synced — move the ones already here out of the folder, or pass `--out-dir` to `ronja app test`.\n",
				filepath.Join(wfdir.StateDirName, appTestDirName))
		}
	}
	return ""
}

// isAppTestArtifact reports whether a resource-relative path's last segment is
// one of the names writeAppTestOutputs writes. The frame name is matched off
// appTestFramePattern rather than a second copy of it, so the two cannot drift.
func isAppTestArtifact(path string) bool {
	base := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		base = path[i+1:]
	}
	if base == appTestReportName || base == appTestSelectedName {
		return true
	}
	prefix, suffix, ok := strings.Cut(appTestFramePattern, "%d")
	if !ok || !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, suffix) {
		return false
	}
	// Prefix and suffix must not overlap in base, or the slice below is out of
	// range — cannot happen with the current pattern, but this function claims
	// to be safe against edits to it.
	if len(base) < len(prefix)+len(suffix) {
		return false
	}
	digits := base[len(prefix) : len(base)-len(suffix)]
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// appTestFile is one artefact this command wrote.
type appTestFile struct {
	Path string `json:"path"`
	// Kind is "report" or "screenshot".
	Kind string `json:"kind"`
	// Phase is the frame's capture phase, empty for the report.
	Phase string `json:"phase,omitempty"`
	// Selected marks the representative frame — the same frame the in-product
	// agent is shown, which is also the one written as screenshot.png.
	Selected bool `json:"selected,omitempty"`
}

// writeAppTestOutputs writes the report and the filmstrip into dir.
//
// A frame that cannot be fetched is a WARNING and nothing more. The observation
// is already in hand and already written; failing the command because one
// picture did not come down would throw away the answer over its illustration.
func writeAppTestOutputs(ctx context.Context, client *api.Client, dir string, outcome *api.PreviewOutcome) ([]appTestFile, error) {
	// 0700, matching the 0600 the files themselves are written at: these are
	// tenant pixels and tenant query results, and a directory this command
	// created should not be the thing that widens them.
	if err := os.MkdirAll(dir, appTestOutDirPerm); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	// The previous run's frames go FIRST, before anything is written.
	//
	// Without this a stale screenshot.png survives a render that captured nothing
	// (or whose frames all failed to fetch), sitting beside a fresh report.json
	// that describes a different render entirely — and a picture of the wrong run
	// is the one artefact here that misleads silently. Only the names this command
	// writes are removed; --out-dir may be a directory somebody keeps other things
	// in.
	if err := clearAppTestFrames(dir); err != nil {
		return nil, err
	}

	reportPath := filepath.Join(dir, appTestReportName)
	// From the RAW bytes, never from a re-marshal — see api.PreviewOutcome.
	if err := writeResultFile(reportPath, string(outcome.Raw)); err != nil {
		return nil, err
	}
	files := []appTestFile{{Path: reportPath, Kind: "report"}}

	for i, shot := range outcome.Result.Screenshots {
		// Both empty, not just the id: DownloadFrame PREFERS the key (one call, on
		// the scope this command already holds) and only falls back to resolving
		// the id. Skipping on a missing id alone dropped a key-only frame that was
		// perfectly fetchable.
		if shot.FileID == "" && shot.FileKey == "" {
			continue
		}
		framePath := filepath.Join(dir, fmt.Sprintf(appTestFramePattern, i+1))
		if err := downloadPreviewFrame(ctx, client, shot, framePath); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not fetch frame %d (%s): %v\n", i+1, frameName(shot), err)
			continue
		}
		files = append(files, appTestFile{
			Path: framePath, Kind: "screenshot", Phase: shot.Phase, Selected: shot.Selected,
		})

		if !shot.Selected {
			continue
		}
		// The representative frame gets a stable second name, so a script or a
		// human never has to work out which number it was — selection is by phase
		// preference, not by position, so "the last one" is not it.
		//
		// Copied from the file just written rather than downloaded again or held
		// in memory: one fetch, no whole-frame buffer, and the two names are
		// guaranteed to be the same pixels.
		selectedPath := filepath.Join(dir, appTestSelectedName)
		if err := copyLocalFile(framePath, selectedPath); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not also write the selected frame as %s: %v\n", selectedPath, err)
			continue
		}
		files = append(files, appTestFile{
			Path: selectedPath, Kind: "screenshot", Phase: shot.Phase, Selected: true,
		})
	}
	return files, nil
}

// clearAppTestFrames removes the frames a PREVIOUS run of this command wrote.
//
// Only the two names this command owns — screenshot.png and screenshot-N.png —
// and nothing else in the directory: --out-dir is a directory somebody named,
// and a command that swept it would be the last time anyone pointed it at a
// place they keep things. (The DEFAULT is this command's own .ronja/test/, but
// the rule is written for the named case, which is the one that can hurt.)
//
// A removal that fails is reported rather than swallowed. The failure it guards
// against is a stale picture read as the current one, and continuing past a
// file that could not be removed would deliver exactly that.
func clearAppTestFrames(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "screenshot-*.png"))
	if err != nil {
		return fmt.Errorf("look for the previous run's frames in %s: %w", dir, err)
	}
	// Delete only names this command itself writes (screenshot-<digits>.png) —
	// the glob also matches e.g. screenshot-final.png, which with an explicit
	// --out-dir can be the user's own file.
	var stale []string
	for _, path := range matches {
		if isAppTestArtifact(filepath.ToSlash(path)) {
			stale = append(stale, path)
		}
	}
	stale = append(stale, filepath.Join(dir, appTestSelectedName))
	for _, path := range stale {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove the previous run's %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

// frameName is what a frame is CALLED in a warning: its id when it has one, its
// storage key when it does not. A frame carrying only a key is fetchable (that
// is the preferred path), so it also has to be nameable when the fetch fails.
func frameName(shot api.PreviewScreenshot) string {
	if shot.FileID != "" {
		return shot.FileID
	}
	return shot.FileKey
}

// downloadPreviewFrame streams one frame to disk at 0600.
func downloadPreviewFrame(ctx context.Context, client *api.Client, shot api.PreviewScreenshot, path string) error {
	body, err := client.DownloadFrame(ctx, shot)
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = writeStreamFile(path, body)
	return err
}

// copyLocalFile duplicates a file this command just wrote, keeping the 0600
// atomic-write guarantees of the original.
func copyLocalFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = writeStreamFile(to, src)
	return err
}
