package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app fork` copies one component-kit file into the folder and changes
// nothing on the server. The path is not a choice: an app file at a kit path
// shadows the kit's copy for the whole bundle, so a file anywhere else shadows
// nothing.
func newDataAppForkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fork <kit-path>",
		Short: "Copy a built-in component-kit file into the folder to customise it",
		Long: `Copy a built-in component-kit file into the folder to customise it.

Data apps compile against an embedded component kit — the shadcn/ui primitives
and Ronja's operator components — which you import as "@/components/ui/button"
and friends. Forking one means having a file of your own at the SAME path:

  ronja app fork components/ui/button.tsx
  ronja app fork components/operator/queue-list.tsx
  ronja app fork lib/utils.ts

The file lands at exactly that path in the folder, byte-identical to what the
compiler currently sees, and from then on it SHADOWS the kit's copy for the
whole bundle. Nothing imports it differently — every existing
"@/components/ui/button" now resolves to your file — so there is nothing else
to change and no way to "unfork" but deleting it again.

This is a local operation. It reads the kit off the instance's public docs
surface and writes one file; it opens no draft, and nothing about the app
changes on the server until you run ` + "`ronja app push`" + `.

An existing file at that path is never overwritten — a fork you already made,
or a file of your own that happens to sit there, is left exactly as it is.

The kit is listed at <instance>/docs/api/kit.md, one line per file.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolderLocally(resolved, wfdir.DataAppKind)
			if err != nil {
				return err
			}

			kitPath, err := normalizeKitPath(args[0])
			if err != nil {
				return err
			}
			// Local refusals come before the request. CheckLocalPaths is clone's
			// gate: a path the folder walk skips would be written once and then
			// invisible to every later command.
			if err := wfdir.CheckLocalPaths([]string{kitPath}, wfdir.DataAppKind); err != nil {
				return err
			}
			full := filepath.Join(f.Root, filepath.FromSlash(kitPath))
			switch _, statErr := os.Lstat(full); {
			case statErr == nil:
				return fmt.Errorf("%s already exists in this folder — that IS the fork (an app file at a kit path shadows the kit's copy), so there is nothing to copy over it.\n  Edit it, or delete it to go back to the built-in component",
					kitPath)
			case !errors.Is(statErr, fs.ErrNotExist):
				return fmt.Errorf("check %s: %w", kitPath, statErr)
			}

			client := api.New(resolved.URL, resolved.Token)
			source, err := client.GetKitFile(cmd.Context(), kitPath)
			if err != nil {
				return err
			}
			// WriteFile validates again, confining the write to this folder.
			if err := wfdir.WriteFile(f.Root, kitPath, source); err != nil {
				return fmt.Errorf("write %s: %w", kitPath, err)
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"root":   f.Root,
					"path":   kitPath,
					"bytes":  len(source),
					"source": resolved.URL + api.KitDocsPath(kitPath),
					"url":    resolved.URL,
				})
			}
			printAppForkReport(kitPath, resolved.URL, len(source))
			return nil
		},
	}
	return cmd
}

// normalizeKitPath turns what an author types (including the "@/" import alias)
// into the kit path the docs route and the folder share. The extension is
// deliberately not guessed: both .ts and .tsx occur in the kit.
func normalizeKitPath(arg string) (string, error) {
	path := strings.TrimSpace(arg)
	path = strings.TrimPrefix(path, "@/")
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", fmt.Errorf("name the kit file to fork, e.g. `ronja app fork components/ui/button.tsx` — the list is at /docs/api/kit.md")
	}
	if filepath.Ext(path) == "" {
		return "", fmt.Errorf("%q has no file extension — kit files are named in full, e.g. components/ui/button.tsx or lib/utils.ts (the list is at /docs/api/kit.md)", arg)
	}
	return path, nil
}

func printAppForkReport(kitPath, url string, bytes int) {
	out := os.Stdout
	fmt.Fprintf(out, "  Forked %s\n\n", kitPath)
	fmt.Fprintf(out, "  From:     %s%s\n", url, api.KitDocsPath(kitPath))
	fmt.Fprintf(out, "  Bytes:    %d\n", bytes)
	fmt.Fprintf(out, "  Shadows:  the built-in component at the same path, for every import of it\n")
	fmt.Fprintf(out, "\n  Nothing was changed on the server.\n")
	fmt.Fprintf(out, "\n  Next: edit %s, then ronja app push\n", kitPath)
}
