package commands

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runQuiet runs the CLI through run, so the exit code is the real one, and
// returns the exit code, stderr and stdout.
func runQuiet(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var code int
	f, read := captureStdout(t)
	stderr := captureStderr(t, func() { code = run(args) })
	return code, stderr, read(f)
}

// commandArgs is the argument vector that reaches cmd, by its canonical names.
func commandArgs(cmd *cobra.Command) []string {
	return strings.Fields(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()))
}

// beforeUsage is an error's own text, without the usage lines guideMisuse
// appends.
func beforeUsage(err error) string {
	text, _, _ := strings.Cut(err.Error(), "\n  usage: ")
	return text
}

func words(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("misuse-word-%d", i)
	}
	return out
}

// arityLimit is the most positionals cmd accepts after accepting some, or -1
// when it has no limit within the first few.
func arityLimit(cmd *cobra.Command) int {
	accepted := false
	for n := 0; n <= 4; n++ {
		ok := cmd.ValidateArgs(words(n)) == nil
		if accepted && !ok {
			return n - 1
		}
		accepted = accepted || ok
	}
	return -1
}

// TestEveryCommandGuidesOnMisuse walks the whole tree. Misuse is caught before
// RunE, so no server is involved.
func TestEveryCommandGuidesOnMisuse(t *testing.T) {
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden || cmd.Deprecated != "" {
			return
		}
		path := cmd.CommandPath()
		args := commandArgs(cmd)
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
		// A runnable group (`ronja propose`) takes its own positionals, so it is
		// held to the leaf checks instead.
		if !cmd.Runnable() {
			for _, misuse := range [][]string{append(args, "zzqx"), append(append([]string{"help"}, args...), "zzqx")} {
				code, stderr, _ := runQuiet(t, misuse...)
				if code == 0 || !strings.Contains(stderr, `"`+path+`"`) || !strings.Contains(stderr, "'"+path+" --help'") {
					t.Errorf("ronja %s: exit %d, stderr %q; want non-zero naming %q and '%s --help'", strings.Join(misuse, " "), code, stderr, path, path)
				}
			}
			code, stderr, stdout := runQuiet(t, append(args, "help")...)
			if code != 0 || stderr != "" || !strings.Contains(stdout, "Available Commands:") {
				t.Errorf("%s help: exit %d, stderr %q; want exit 0 with its command list", path, code, stderr)
			}
			return
		}
		if !cmd.DisableFlagParsing {
			code, stderr, _ := runQuiet(t, append(args, "--definitely-not-a-flag")...)
			if code == 0 || !strings.HasPrefix(stderr, "error: unknown flag: --definitely-not-a-flag\n") ||
				!strings.Contains(stderr, "'"+path+" --help'") {
				t.Errorf("%s --definitely-not-a-flag: exit %d, stderr %q; want cobra's line, then '%s --help'", path, code, stderr, path)
			}
		}
		if limit := arityLimit(cmd); limit >= 0 {
			code, stderr, _ := runQuiet(t, append(args, words(limit+1)...)...)
			first, _, _ := strings.Cut(stderr, "\n")
			if code == 0 || strings.Contains(first, "usage:") || !strings.Contains(stderr, "\n  usage: "+cmd.UseLine()) ||
				strings.Count(stderr, "usage: ") != 1 {
				t.Errorf("%s with %d args: exit %d, stderr %q; want its own first line, then one usage line %q", path, limit+1, code, stderr, cmd.UseLine())
			}
		}
	}
	for _, cmd := range newRootCmd().Commands() {
		walk(cmd)
	}
}

func TestMisuseKeepsCobrasFirstLine(t *testing.T) {
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	cases := []struct {
		args            []string
		first, contains string
	}{
		{[]string{"wf", "--profile", "foo", "stauts"}, `error: unknown command "stauts" for "ronja workflow" — did you mean "status"? Run 'ronja workflow --help' for its commands`, ""},
		{[]string{"sync", "--dir", "x", "stauts"}, `error: unknown command "stauts" for "ronja sync" — did you mean "status"? Run 'ronja sync --help' for its commands`, ""},
		{[]string{"wf", "help", "stauts"}, `error: unknown command "stauts" for "ronja workflow" — did you mean "status"? Run 'ronja workflow --help' for its commands`, ""},
		{[]string{"help", "wf", "stauts"}, `error: unknown command "stauts" for "ronja workflow" — did you mean "status"? Run 'ronja workflow --help' for its commands`, ""},
		{[]string{"help", "tabel"}, `error: unknown command "tabel" for "ronja"`, unknownRootHint},
		{[]string{"workflow", "clone"}, "error: accepts between 1 and 2 arg(s), received 0", "\n  usage: ronja workflow clone"},
		{[]string{"api"}, "error: accepts 1 arg(s), received 0", "\n  see 'ronja api --help'"},
		{[]string{"workflow", "status", "--jsn"}, "error: unknown flag: --jsn", "did you mean --json?"},
		{[]string{"login", "--url"}, "error: flag needs an argument: --url", "Ronja instance to talk to"},
		{[]string{"workflow", "stauts"}, `error: unknown command "stauts" for "ronja workflow" — did you mean "status"? Run 'ronja workflow --help' for its commands`, ""},
		{[]string{"wf", "stauts", "--timeout", "600"}, `error: unknown command "stauts" for "ronja workflow" — did you mean "status"? Run 'ronja workflow --help' for its commands`, ""},
		{[]string{"wf", "tets", "--write-live"}, `error: unknown command "tets" for "ronja workflow" — did you mean "test"? Run 'ronja workflow --help' for its commands`, ""},
		{[]string{"wf", "--bogus"}, "error: unknown flag: --bogus", "\n  see 'ronja workflow --help' for the flags it takes"},
		{[]string{"workflow", "zzqx"}, `error: unknown command "zzqx" for "ronja workflow" — run 'ronja workflow --help' for its commands`, ""},
		{[]string{"table"}, `error: unknown command "table" for "ronja"`, "\nronja's commands sync folders; reads and other calls go through ronja api — see ronja api /docs/api/endpoints.md, or ronja --help\n"},
		{[]string{"logn"}, `error: unknown command "logn" for "ronja"`, "Did you mean this?\n\tlogin\n"},
	}
	for _, c := range cases {
		code, stderr, _ := runQuiet(t, c.args...)
		first, _, _ := strings.Cut(stderr, "\n")
		if code != 1 || first != c.first || !strings.Contains(stderr, c.contains) || strings.Count(stderr, "see '") > 1 {
			t.Errorf("ronja %s: exit %d, stderr %q; want exit 1, first line %q, containing %q, at most one see line", strings.Join(c.args, " "), code, stderr, c.first, c.contains)
		}
	}
	// Shell completion asks about partial words, so it is never refused.
	if code, stderr, _ := runQuiet(t, "__complete", "wf", "stauts"); code != 0 || strings.Contains(stderr, "unknown command") {
		t.Errorf("ronja __complete wf stauts: exit %d, stderr %q; want exit 0, no refusal", code, stderr)
	}
}

// A bare group still prints its help and exits 0, and --help after an unknown
// verb is still a request for help.
func TestBareGroupStillPrintsHelp(t *testing.T) {
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	for _, args := range [][]string{{"workflow"}, {"wf"}, {"secret"}, {"workflow", "stauts", "--help"}, {"wf", "help"}, {"help", "wf"}, {"wf", "help", "status"}} {
		code, stderr, stdout := runQuiet(t, args...)
		if code != 0 || stderr != "" || !strings.Contains(stdout, "Available Commands:") {
			t.Errorf("ronja %s: exit %d, stderr %q, stdout has no command list", strings.Join(args, " "), code, stderr)
		}
	}
}

// A validator that already explains keeps its text first, then gains the
// usage line and one see line.
func TestExplainingValidatorGainsUsageAndOneSeeLine(t *testing.T) {
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	code, stderr, _ := runQuiet(t, "wf", "status", dir)
	want := "error: ronja workflow status reads the folder you are in — run it there: cd " + dir + " && ronja workflow status\n" +
		"  usage: ronja workflow status [flags]\n" +
		"  see 'ronja workflow status --help'\n"
	if code != 1 || stderr != want {
		t.Errorf("ronja wf status <dir>: exit %d, stderr %q; want exit 1, stderr %q", code, stderr, want)
	}
}
