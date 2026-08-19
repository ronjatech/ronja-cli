package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// What a render leaves on disk: the report, the filmstrip, and the removal of
// the previous run's frames before any of it is written. Everything here owns
// --out-dir — which names it writes, which it clears, and which failures are a
// warning rather than the end of the command.

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
// and nothing else in the directory: --out-dir defaults to the working
// directory, which is somebody's app folder, and a command that swept it would
// be the last time anyone passed the default.
//
// A removal that fails is reported rather than swallowed. The failure it guards
// against is a stale picture read as the current one, and continuing past a
// file that could not be removed would deliver exactly that.
func clearAppTestFrames(dir string) error {
	stale, err := filepath.Glob(filepath.Join(dir, "screenshot-*.png"))
	if err != nil {
		return fmt.Errorf("look for the previous run's frames in %s: %w", dir, err)
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
