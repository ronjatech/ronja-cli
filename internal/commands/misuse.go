package commands

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// unknownRootHint is the CLI's design rule, stated where an agent meets it.
const unknownRootHint = "ronja's commands sync folders; reads and other calls go through ronja api — see ronja api /docs/api/endpoints.md, or ronja --help"

// refuseUnknownCommand resolves args the way cobra is about to and refuses a
// word that names no command. Cobra prints a group's help and exits 0 for one,
// and at the root says only "unknown command". It runs before ExecuteContext
// so the update check never starts for a mistyped command. `help <words…>` and
// `<group> help <words…>` are held to the words they ask about, so a mistyped
// one is refused while a real one still prints help.
func refuseUnknownCommand(root *cobra.Command, args []string) error {
	if args == nil {
		args = os.Args[1:]
	}
	if len(args) > 0 && (args[0] == cobra.ShellCompRequestCmd || args[0] == cobra.ShellCompNoDescRequestCmd) {
		return nil
	}
	cmd, rest, err := root.Find(args)
	if err != nil {
		if cmd == root {
			return fmt.Errorf("%s\n%s", strings.TrimRight(err.Error(), "\n"), unknownRootHint)
		}
		return nil
	}
	if cmd.Name() == "help" && cmd.Parent() == root {
		return refuseUnknownCommand(root, rest)
	}
	if cmd == root || cmd.Runnable() || !cmd.HasSubCommands() {
		return nil
	}
	// A group runs nothing, so parsing its flags here has no effect beyond
	// this check. A flag the group does not take may belong to the mistyped
	// verb before it, so a leading word is still the verb; a leading flag
	// error is left for cobra to report.
	cmd.InitDefaultHelpFlag()
	var words []string
	if cmd.ParseFlags(rest) != nil {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return nil
		}
		words = rest
	} else {
		words = cmd.Flags().Args()
		if help, _ := cmd.Flags().GetBool("help"); help || len(words) == 0 {
			return nil
		}
	}
	if words[0] == "help" {
		path := strings.Fields(strings.TrimPrefix(cmd.CommandPath(), root.Name()))
		return refuseUnknownCommand(root, append(path, words[1:]...))
	}
	path := cmd.CommandPath()
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	next := "run"
	if near := cmd.SuggestionsFor(words[0]); len(near) > 0 {
		for i := range near {
			near[i] = strconv.Quote(near[i])
		}
		next = "did you mean " + strings.Join(near, " or ") + "? Run"
	}
	return fmt.Errorf("unknown command %q for %q — %s '%s --help' for its commands", words[0], path, next, path)
}

// guideMisuse makes every usage error in the tree say what to do next: an
// arity error gains the command's usage line, a flag error the flag's own
// help or the closest flag. Cobra's first line stays as it was, since scripts
// match it.
func guideMisuse(root *cobra.Command) {
	root.SetFlagErrorFunc(guideFlagError)
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if validate := cmd.Args; validate != nil {
			cmd.Args = func(cmd *cobra.Command, args []string) error {
				return withUsage(cmd, validate(cmd, args))
			}
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// withUsage gives an argument error the usage line and the see line, each
// unless the error already carries it.
func withUsage(cmd *cobra.Command, err error) error {
	if err == nil {
		return err
	}
	var tail string
	if usage := "\n  usage: "; !strings.Contains(err.Error(), usage) {
		tail += usage + cmd.UseLine()
	}
	if see := seeHelp(cmd); !strings.Contains(err.Error(), see) {
		tail += "\n  " + see
	}
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w%s", err, tail)
}

func seeHelp(cmd *cobra.Command) string {
	return "see '" + cmd.CommandPath() + " --help'"
}

func guideFlagError(cmd *cobra.Command, err error) error {
	var (
		hint       string
		unknown    *pflag.NotExistError
		needsValue *pflag.ValueRequiredError
		invalid    *pflag.InvalidValueError
	)
	switch {
	case errors.As(err, &unknown):
		if unknown.GetSpecifiedShortnames() == "" {
			if near := closestFlag(cmd, unknown.GetSpecifiedName()); near != "" {
				hint = "\n  did you mean --" + near + "?"
			}
		}
	case errors.As(err, &needsValue):
		hint = flagHelp(needsValue.GetFlag())
	case errors.As(err, &invalid):
		hint = flagHelp(invalid.GetFlag())
	}
	return fmt.Errorf("%w%s\n  %s for the flags it takes", err, hint, seeHelp(cmd))
}

func flagHelp(f *pflag.Flag) string {
	if f == nil {
		return ""
	}
	name := "--" + f.Name
	if f.Shorthand != "" {
		name = "-" + f.Shorthand + ", " + name
	}
	if kind, usage := pflag.UnquoteUsage(f); usage != "" {
		if kind != "" {
			name += " " + kind
		}
		return "\n  " + name + ": " + usage
	}
	return ""
}

// closestFlag is the flag cmd accepts, inherited ones included, nearest to
// name within an edit distance of 2, or "".
func closestFlag(cmd *cobra.Command, name string) string {
	best, bestDist := "", 3
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Deprecated != "" {
			return
		}
		if d := editDistance(strings.ToLower(name), strings.ToLower(f.Name)); d < bestDist {
			best, bestDist = f.Name, d
		}
	})
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
