package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The commands that read the folder you are in. Given that folder as an
// argument, each says to run it from inside it.
var folderCommands = [][]string{
	{"wf", "status"}, {"wf", "push"}, {"wf", "publish"}, {"wf", "discard"}, {"wf", "validate"}, {"wf", "run"}, {"wf", "test"},
	{"app", "status"}, {"app", "push"}, {"app", "publish"}, {"app", "discard"}, {"app", "validate"}, {"app", "test"},
	{"module", "status"}, {"module", "push"}, {"module", "publish"}, {"module", "discard"}, {"module", "validate"},
	{"automation", "status"}, {"automation", "push"},
	{"pipeline", "status"},
	{"bind"},
}

// The folder commands that take their folder as --dir.
var dirFlagCommands = [][]string{
	{"sync", "status"}, {"sync", "apply"}, {"sync", "check"},
	{"db", "migrate", "status"}, {"db", "migrate", "push"},
}

func folderArgWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sync-meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFolderCommandsSayToRunInsideAGivenFolder(t *testing.T) {
	for _, words := range folderCommands {
		root := newRootCmd()
		cmd, _, err := root.Find(words)
		if err != nil {
			t.Fatalf("%v: %v", words, err)
		}
		path := cmd.CommandPath()
		_, err = runCLI(t, folderArgWorkdir(t), append(append([]string(nil), words...), "sync-meta")...)
		want := path + " reads the folder you are in — run it there: cd sync-meta && " + path
		if err == nil || beforeUsage(err) != want {
			t.Errorf("%s sync-meta = %v\n  want %q", path, err, want)
		}
	}
	for _, words := range dirFlagCommands {
		root := newRootCmd()
		cmd, _, err := root.Find(words)
		if err != nil {
			t.Fatalf("%v: %v", words, err)
		}
		path := cmd.CommandPath()
		_, err = runCLI(t, folderArgWorkdir(t), append(append([]string(nil), words...), "sync-meta")...)
		want := path + " takes its folder as --dir, not an argument: " + path + " --dir sync-meta"
		if err == nil || beforeUsage(err) != want {
			t.Errorf("%s sync-meta = %v\n  want %q", path, err, want)
		}
	}
}

// Negative control: an argument that is not an existing directory keeps the
// error the command gave before.
func TestFolderCommandsKeepTheirErrorForANonFolder(t *testing.T) {
	for _, words := range append(append([][]string(nil), folderCommands...), dirFlagCommands...) {
		_, err := runCLI(t, folderArgWorkdir(t), append(append([]string(nil), words...), "no-such-folder")...)
		if err == nil {
			t.Errorf("%v no-such-folder was accepted", words)
			continue
		}
		if strings.Contains(err.Error(), "cd no-such-folder") || strings.Contains(err.Error(), "--dir no-such-folder") {
			t.Errorf("%v no-such-folder got the folder hint: %v", words, err)
		}
		if words[0] == "wf" && words[1] == "run" {
			if !strings.Contains(err.Error(), "takes no workflow id") {
				t.Errorf("wf run lost its own refusal: %v", err)
			}
		} else if !strings.HasPrefix(err.Error(), `unknown command "no-such-folder" for "ronja `) {
			t.Errorf("%v no-such-folder = %v, want cobra's unknown-command error", words, err)
		}
	}
}

// The retired --env refusal still comes first on db migrate.
func TestMigrateStillRefusesARetiredEnvBeforeTheFolderHint(t *testing.T) {
	_, err := runCLI(t, folderArgWorkdir(t), "db", "migrate", "status", "--env", "dev", "sync-meta")
	if err == nil || !strings.Contains(err.Error(), "Dev copies of databases were removed") {
		t.Errorf("db migrate status --env dev sync-meta = %v, want the retired-env refusal", err)
	}
}

// Every leaf under a folder group that refuses an existing folder as its
// argument must refuse it with the folder hint. It calls the validator rather
// than comparing it to folderNoArgs, so it holds however the validators are
// wrapped.
func TestEveryFolderGroupLeafGivenAFolderGivesTheHint(t *testing.T) {
	dir := t.TempDir()
	root := newRootCmd()
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if subs := cmd.Commands(); len(subs) > 0 {
			for _, sub := range subs {
				walk(sub)
			}
			return
		}
		err := cmd.ValidateArgs([]string{dir})
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "reads the folder you are in — run it there: cd "+dir) &&
			!strings.Contains(err.Error(), "takes its folder as --dir, not an argument") {
			t.Errorf("%s <folder> = %v, want the folder hint", cmd.CommandPath(), err)
		}
	}
	for _, group := range []string{"wf", "app", "module", "automation", "pipeline", "sync", "db", "bind"} {
		cmd, _, err := root.Find([]string{group})
		if err != nil {
			t.Fatalf("%s: %v", group, err)
		}
		walk(cmd)
	}
}

// An argument naming the folder you are in needs no cd or --dir: the hint says
// to drop it. A path that needs quoting is quoted in either form, so the line
// can be pasted.
func TestFolderHintDropsTheCurrentFolderAndQuotesAPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "my folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"wf", "push", "."}, "ronja workflow push reads the folder you are in, and that is the folder you named — drop the argument: ronja workflow push"},
		{[]string{"sync", "status", "./"}, "ronja sync status takes its folder as --dir, not an argument, and the folder you are in is the default — drop the argument: ronja sync status"},
		{[]string{"wf", "push", "my folder"}, "ronja workflow push reads the folder you are in — run it there: cd 'my folder' && ronja workflow push"},
		{[]string{"sync", "status", "my folder"}, "ronja sync status takes its folder as --dir, not an argument: ronja sync status --dir 'my folder'"},
	} {
		_, err := runCLI(t, dir, c.args...)
		if err == nil || beforeUsage(err) != c.want {
			t.Errorf("ronja %q = %v\n  want %q", c.args, err, c.want)
		}
	}
}
