package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/dbdir"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// TestMigrateStatusNamesProposeWhenPending: a status with migrations pending
// ends, on stderr, with the propose command first and push second, each with
// the caller's own flags. No line with nothing pending or with drift (both
// would be refused), and --json is untouched: the deploy job parses it.
func TestMigrateStatusNamesProposeWhenPending(t *testing.T) {
	pending := map[string]any{"databaseID": "mdb-abc", "dryRun": true, "results": []map[string]any{
		{"name": "0001_leads", "outcome": "skipped", "seq": 1},
		{"name": "0002_score", "outcome": "pending"},
	}}
	const counts = "  1 applied, 1 pending, 0 drifted (nothing was changed)\n"
	statusBound := func(t *testing.T, ledger map[string]any, workdir string, bind func(url string), args ...string) (string, string, error) {
		t.Helper()
		srv := newDatabaseServer(t)
		srv.migrations = ledger
		signInTo(t, srv.server.URL)
		bind(srv.server.URL)
		var out string
		var err error
		stderr := captureStderr(t, func() {
			out, err = runCLI(t, workdir, append([]string{"db", "migrate", "status"}, args...)...)
		})
		return out, stderr, err
	}
	status := func(t *testing.T, ledger map[string]any, workdir string, args ...string) (string, string, error) {
		t.Helper()
		return statusBound(t, ledger, workdir, func(string) {}, args...)
	}

	t.Run("from the binding, with --stack and --dir", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeLocal(t, repo, "db/crm/migrations/0001_leads.sql", "CREATE TABLE leads (id int);\n")
		writeLocal(t, repo, "db/crm/migrations/0002_score.sql", "ALTER TABLE leads ADD score int;\n")
		bind := func(url string) {
			if err := (&dbdir.Binding{}).SaveStack(filepath.Join(repo, "db/crm"), wfdir.InstanceKey{URL: url}, "dev", "mdb-abc"); err != nil {
				t.Fatal(err)
			}
		}
		out, stderr, err := statusBound(t, pending, repo, bind, "--stack", "dev", "--dir", "db/crm")
		if err != nil || out != "" {
			t.Fatalf("status: %v, stdout %q\n%s", err, out, stderr)
		}
		want := "  Recommended for agents: ronja propose --stack dev db/crm, then the step it names; the person this login belongs to approves the change list in Ronja, which then applies exactly the SQL it shows. To apply it yourself now: ronja db migrate push --stack dev --dir db/crm\n"
		if !strings.HasSuffix(stderr, counts+"\n"+want) {
			t.Fatalf("stderr does not end with the counts and the line:\n%s", stderr)
		}
	})

	t.Run("with --database", func(t *testing.T) {
		out, stderr, err := status(t, pending, migrationsDir(t), "--database", "mdb-abc")
		if err != nil || out != "" {
			t.Fatalf("status: %v, stdout %q\n%s", err, out, stderr)
		}
		want := "  Recommended for agents: ronja propose --database mdb-abc, then the step it names; the person this login belongs to approves the change list in Ronja, which then applies exactly the SQL it shows. To apply it yourself now: ronja db migrate push --database mdb-abc\n"
		if !strings.HasSuffix(stderr, counts+"\n"+want) {
			t.Fatalf("stderr does not end with the counts and the line:\n%s", stderr)
		}
	})

	t.Run("after the server's notes", func(t *testing.T) {
		withNote := map[string]any{"databaseID": "mdb-abc", "dryRun": true, "notes": []string{"a note from the server"}, "results": pending["results"]}
		_, stderr, err := status(t, withNote, migrationsDir(t), "--database", "mdb-abc")
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
		if !strings.HasPrefix(lines[len(lines)-1], "  Recommended for agents: ronja propose --database mdb-abc, then the step it names;") || !strings.Contains(stderr, "  Note: a note from the server\n") {
			t.Fatalf("the line is not the last of stderr, after the note:\n%s", stderr)
		}
	})

	t.Run("more pending than one push applies", func(t *testing.T) {
		results := []map[string]any{}
		for i := 1; i <= maxPendingPerPush+1; i++ {
			results = append(results, map[string]any{"name": fmt.Sprintf("%04d_step", i), "outcome": "pending"})
		}
		_, stderr, err := status(t, map[string]any{"databaseID": "mdb-abc", "dryRun": true, "results": results}, migrationsDir(t), "--database", "mdb-abc")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, "propose") {
			t.Fatalf("a status push would refuse named propose:\n%s", stderr)
		}
	})

	t.Run("nothing pending", func(t *testing.T) {
		_, stderr, err := status(t, map[string]any{"databaseID": "mdb-abc", "dryRun": true, "results": []map[string]any{
			{"name": "0001_leads", "outcome": "skipped", "seq": 1},
		}}, migrationsDir(t), "--database", "mdb-abc")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, "propose") || !strings.HasSuffix(stderr, "  1 applied, 0 pending, 0 drifted (nothing was changed)\n") {
			t.Fatalf("stderr = %q, want the counts last and no propose line", stderr)
		}
	})

	t.Run("drift with pending", func(t *testing.T) {
		_, stderr, err := status(t, map[string]any{"databaseID": "mdb-abc", "dryRun": true, "results": []map[string]any{
			{"name": "0001_leads", "outcome": "drifted", "seq": 1},
			{"name": "0002_score", "outcome": "pending"},
		}}, migrationsDir(t), "--database", "mdb-abc")
		if err == nil || !strings.Contains(err.Error(), "0001_leads") {
			t.Fatalf("status over drift: %v, want the drift error", err)
		}
		if strings.Contains(stderr, "propose") {
			t.Fatalf("a drifted status named propose:\n%s", stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		out, stderr, err := status(t, pending, migrationsDir(t), "--database", "mdb-abc", "--json")
		if err != nil {
			t.Fatal(err)
		}
		const want = `{
  "databaseID": "mdb-abc",
  "dryRun": true,
  "results": [
    {
      "name": "0001_leads",
      "outcome": "skipped",
      "seq": 1
    },
    {
      "name": "0002_score",
      "outcome": "pending",
      "seq": 0
    }
  ],
  "notes": null
}
`
		if out != want {
			t.Errorf("stdout changed:\n%s", out)
		}
		if strings.Contains(stderr, "propose") {
			t.Errorf("--json printed the propose line on stderr:\n%s", stderr)
		}
	})
}

// TestMigrateHelpNamesPropose: an agent reading --json never sees the status
// line, so the help of every command it would deploy with is where it learns
// that `ronja propose` is its way to ship.
func TestMigrateHelpNamesPropose(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{
		{"db", "migrate"}, {"db", "migrate", "push"}, {"db", "migrate", "status"},
		{"app", "publish"}, {"module", "publish"}, {"wf", "publish"}, {"pipeline", "publish"}, {"sync", "apply"},
		{"propose"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Fatalf("%s: %v", strings.Join(path, " "), err)
		}
		if !strings.Contains(cmd.Long, "Agents: ship with `ronja propose`") || !strings.Contains(cmd.Long, "recommended") {
			t.Errorf("ronja %s --help does not open its approval paragraph with \"Agents: ship with `ronja propose`\" as the recommended way", strings.Join(path, " "))
		}
	}
	propose, _, _ := root.Find([]string{"propose"})
	if !strings.Contains(propose.Long, "--database") || propose.Flags().Lookup("database") == nil {
		t.Error("ronja propose --help does not document --database")
	}
}

// TestMigrateNextStep: the line pastes into a shell as typed, and a folder
// whose name starts with "-" cannot read as a flag of propose.
func TestMigrateNextStep(t *testing.T) {
	for _, c := range []struct{ dir, propose, push string }{
		{".", "ronja propose", "ronja db migrate push"},
		{"db/crm", "ronja propose db/crm", "ronja db migrate push --dir db/crm"},
		{"my db", "ronja propose 'my db'", "ronja db migrate push --dir 'my db'"},
		{"it's", `ronja propose 'it'\''s'`, `ronja db migrate push --dir 'it'\''s'`},
		{"-x", "ronja propose ./-x", "ronja db migrate push --dir -x"},
		{"/abs/-x", "ronja propose /abs/-x", "ronja db migrate push --dir /abs/-x"},
	} {
		got := migrateNextStep(migrateOpts{dir: c.dir})
		want := "Recommended for agents: " + c.propose + ", then the step it names; the person this login belongs to approves the change list in Ronja, which then applies exactly the SQL it shows. To apply it yourself now: " + c.push
		if got != want {
			t.Errorf("dir %q:\n got %s\nwant %s", c.dir, got, want)
		}
	}

	// A typed --profile or --url is what made status read this database, so
	// both commands carry it; without it they would run on the default profile.
	for _, c := range []struct {
		name          string
		profile, url  string
		propose, push string
	}{
		{"--profile", "acme", "", "ronja propose --profile acme --database mdb-abc", "ronja db migrate push --profile acme --database mdb-abc"},
		{"--url", "", "https://acme.example.com", "ronja propose --url https://acme.example.com --database mdb-abc", "ronja db migrate push --url https://acme.example.com --database mdb-abc"},
		{"both", "my profile", "https://acme.example.com", "ronja propose --profile 'my profile' --url https://acme.example.com --database mdb-abc", "ronja db migrate push --profile 'my profile' --url https://acme.example.com --database mdb-abc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			savedProfile, savedURL := flagProfile, flagURL
			t.Cleanup(func() { flagProfile, flagURL = savedProfile, savedURL })
			flagProfile, flagURL = c.profile, c.url
			got := migrateNextStep(migrateOpts{database: "mdb-abc"})
			want := "Recommended for agents: " + c.propose + ", then the step it names; the person this login belongs to approves the change list in Ronja, which then applies exactly the SQL it shows. To apply it yourself now: " + c.push
			if got != want {
				t.Errorf("\n got %s\nwant %s", got, want)
			}
		})
	}
}
