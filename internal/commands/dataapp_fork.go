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

// `ronja app fork` copies one component-kit file into the folder.
//
// LOCAL-FIRST, and that is the whole design: it writes a file and changes
// NOTHING on the server. No draft is opened, no row is touched, and a fork you
// think better of is `rm` — not a discard.
//
// It is a sync verb rather than a resource verb, on the same doctrine as the
// rest of this tree. It does not list the kit, browse it or address a row; it
// moves bytes between the instance and the folder, which is the one thing plain
// HTTP does badly here: an author forking a component over curl has to know the
// docs route, know the folder path is the SAME path, and get both right in one
// go, with a silent wrong answer (a file at the wrong path is just a new file)
// if they do not.
//
// Shadowing is what makes this work at all. A data app compiles against the
// embedded kit as a base layer, and an app file at the same path replaces the
// kit's for the whole bundle — so `@/components/ui/button` keeps resolving,
// from every file that imports it, with no import to update anywhere. Which is
// also why the path is not a choice: fork it somewhere else and the kit's
// version is still what everything compiles against.
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
			// Every refusal that can be made locally is made before the request.
			// A path this folder cannot hold does not become holdable after a
			// round trip, and this is the form CI runs.
			//
			// CheckLocalPaths is the same gate clone applies to server-supplied
			// paths, and for the same reason: a path the walk would never see
			// again (a dot-directory, one of this kind's skipped directories) is
			// a file written once and then invisible to every later command.
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
			// Validated once more inside WriteFile, which is what keeps a path
			// that arrived from outside from writing anywhere but this folder.
			if err := wfdir.WriteFile(f.Root, kitPath, source); err != nil {
				return fmt.Errorf("write %s: %w", kitPath, err)
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"root":  f.Root,
					"path":  kitPath,
					"bytes": len(source),
					// Where the bytes came from, so a caller can go and read the
					// same source without knowing the route family.
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

// normalizeKitPath turns what an author is likely to type into the kit path the
// docs route and the folder both use.
//
// The alias form is the one worth accepting: the kit is imported as
// "@/components/ui/button", so that string is what is on screen when someone
// decides to fork it. The EXTENSION is deliberately not guessed — ".ts" and
// ".tsx" both occur in the kit, and a wrong guess would fetch a file that
// exists and shadow nothing.
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
