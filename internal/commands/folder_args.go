package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// folderNoArgs is cobra.NoArgs for a command that works on the folder you are
// in. Given an existing directory it says to run the command there, rather
// than "unknown command". It does not run there itself: these commands also
// resolve relative paths in other flags (--out, -d @file), which would quietly
// resolve against a different folder.
func folderNoArgs(cmd *cobra.Command, args []string) error {
	if err := folderArgHint(cmd, args); err != nil {
		return err
	}
	return cobra.NoArgs(cmd, args)
}

// folderArgHint returns the hint for a first argument that is an existing
// directory, or nil.
func folderArgHint(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	fi, err := os.Stat(args[0])
	if err != nil || !fi.IsDir() {
		return nil
	}
	here := false
	if cwd, err := os.Stat("."); err == nil {
		here = os.SameFile(fi, cwd)
	}
	path := cmd.CommandPath()
	if cmd.Flags().Lookup("dir") != nil {
		if here {
			return fmt.Errorf("%s takes its folder as --dir, not an argument, and the folder you are in is the default — drop the argument: %s", path, path)
		}
		return fmt.Errorf("%s takes its folder as --dir, not an argument: %s --dir %s", path, path, shellWord(args[0]))
	}
	if here {
		return fmt.Errorf("%s reads the folder you are in, and that is the folder you named — drop the argument: %s", path, path)
	}
	return fmt.Errorf("%s reads the folder you are in — run it there: cd %s && %s", path, shellWord(args[0]), path)
}

// shellWord is s as one shell word: unchanged when no character in it is
// special to a shell, single-quoted otherwise.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./-+:@,") == "" {
		return s
	}
	return shellQuote(s)
}
