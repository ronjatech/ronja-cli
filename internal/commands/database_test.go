package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

func TestParseSQLParams(t *testing.T) {
	t.Run("empty means none", func(t *testing.T) {
		got, err := parseSQLParams("")
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("whitespace means none", func(t *testing.T) {
		got, err := parseSQLParams("   ")
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("decodes a mixed array", func(t *testing.T) {
		got, err := parseSQLParams(`["a@b.c", 42, true, null]`)
		if err != nil {
			t.Fatalf("parseSQLParams: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("got %d params, want 4", len(got))
		}
		want := []string{`"a@b.c"`, `42`, `true`, `null`}
		for i, w := range want {
			if string(got[i]) != w {
				t.Errorf("param %d = %s, want %s", i, got[i], w)
			}
		}
	})
	t.Run("malformed names the flag", func(t *testing.T) {
		_, err := parseSQLParams(`[not json`)
		if err == nil {
			t.Fatal("want an error for malformed JSON")
		}
		if !strings.Contains(err.Error(), "--params") {
			t.Fatalf("error should name the flag, got: %v", err)
		}
	})
	t.Run("a bare scalar is refused", func(t *testing.T) {
		if _, err := parseSQLParams(`"a@b.c"`); err == nil {
			t.Fatal("want an error for a scalar — params is an array")
		}
	})
}

// A CI gate relies on drift exiting non-zero rather than parsing output.
func TestReportMigrationsFailsOnDrift(t *testing.T) {
	err := reportMigrations(&api.MigrationSetResult{
		DatabaseID: "mdb-abc",
		DryRun:     true,
		Results: []api.MigrationResult{
			{Name: "0001_leads", Outcome: api.MigrationSkipped, Seq: 1},
			{Name: "0002_score", Outcome: api.MigrationDrifted, Seq: 2},
		},
	})
	if err == nil {
		t.Fatal("drift must be an error — a status that reports it quietly is not a gate")
	}
	if !strings.Contains(err.Error(), "0002_score") {
		t.Fatalf("error must name the drifted migration, got: %v", err)
	}
	if strings.Contains(err.Error(), "0001_leads") {
		t.Fatalf("error should name only what drifted, got: %v", err)
	}
}

func TestReportMigrationsCleanRuns(t *testing.T) {
	t.Run("dry run with pending work", func(t *testing.T) {
		err := reportMigrations(&api.MigrationSetResult{
			DryRun: true,
			Results: []api.MigrationResult{
				{Name: "0001_leads", Outcome: api.MigrationSkipped, Seq: 1},
				{Name: "0002_score", Outcome: api.MigrationPending},
			},
		})
		if err != nil {
			t.Fatalf("pending work is not a failure: %v", err)
		}
	})
	t.Run("applied", func(t *testing.T) {
		err := reportMigrations(&api.MigrationSetResult{
			Results: []api.MigrationResult{{Name: "0001_leads", Outcome: api.MigrationApplied, Seq: 1}},
		})
		if err != nil {
			t.Fatalf("a successful apply is not a failure: %v", err)
		}
	})
	t.Run("re-push is all skipped", func(t *testing.T) {
		err := reportMigrations(&api.MigrationSetResult{
			Results: []api.MigrationResult{{Name: "0001_leads", Outcome: api.MigrationSkipped, Seq: 1}},
		})
		if err != nil {
			t.Fatalf("an idempotent re-push is not a failure: %v", err)
		}
	})
}

func TestParseEnvFlag(t *testing.T) {
	t.Run("accepted values", func(t *testing.T) {
		cases := map[string]api.Environment{
			"":     api.EnvironmentProd, // the flag's own default
			"prod": api.EnvironmentProd,
			"dev":  api.EnvironmentDev,
			"DEV":  api.EnvironmentDev,
			" dev": api.EnvironmentDev,
		}
		for in, want := range cases {
			got, err := parseEnvFlag(in)
			if err != nil {
				t.Errorf("parseEnvFlag(%q): %v", in, err)
				continue
			}
			if got != want {
				t.Errorf("parseEnvFlag(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("a typo names the flag", func(t *testing.T) {
		// "development" must not run against production for not being "dev".
		for _, in := range []string{"prd", "development", "staging"} {
			err := func() error { _, err := parseEnvFlag(in); return err }()
			if err == nil {
				t.Fatalf("parseEnvFlag(%q) must be refused", in)
			}
			if !strings.Contains(err.Error(), "--env") {
				t.Errorf("error should name the flag, got: %v", err)
			}
			if !strings.Contains(err.Error(), in) {
				t.Errorf("error should quote what was typed, got: %v", err)
			}
		}
	})
}

func TestReportEnvironmentNamesTheDevCopy(t *testing.T) {
	stderr := captureStderr(t, func() {
		reportEnvironment(api.EnvironmentDev, "dev", "mdb-dev-1", "mdb-abc")
	})
	if !strings.Contains(stderr, "DEV COPY") || !strings.Contains(stderr, "mdb-abc") {
		t.Fatalf("a dev run must say so, naming the PRODUCTION database: %q", stderr)
	}
	if strings.Contains(stderr, "mdb-dev-1") {
		t.Errorf("the copy's own id is one the caller has never seen: %q", stderr)
	}

	quiet := captureStderr(t, func() {
		reportEnvironment(api.EnvironmentProd, "prod", "mdb-abc", "")
	})
	if quiet != "" {
		t.Errorf("a production run must print nothing: %q", quiet)
	}
}

// An instance that predates ?environment=dev answers from production without
// complaint; the server-side gate cannot catch that.
func TestReportEnvironmentWarnsWhenProductionAnswered(t *testing.T) {
	for _, answered := range []string{"prod", ""} {
		stderr := captureStderr(t, func() {
			reportEnvironment(api.EnvironmentDev, answered, "mdb-abc", "")
		})
		if !strings.Contains(stderr, "PRODUCTION answered") {
			t.Errorf("environment=%q: asking for dev and getting production must be called out: %q", answered, stderr)
		}
	}
}

// The environment line has exactly one writer (reportEnvironment), so it is
// never printed twice or dropped in --json mode.
func TestReportMigrationsSaysNothingAboutTheEnvironment(t *testing.T) {
	stderr := captureStderr(t, func() {
		_ = reportMigrations(&api.MigrationSetResult{
			DatabaseID:       "mdb-dev-1",
			Environment:      "dev",
			ParentDatabaseID: "mdb-abc",
			Results:          []api.MigrationResult{{Name: "0001_leads", Outcome: api.MigrationApplied, Seq: 1}},
		})
	})
	if strings.Contains(stderr, "DEV COPY") {
		t.Errorf("reportMigrations must leave the environment line to reportEnvironment: %q", stderr)
	}
}

// A note is advisory, never a failure: only the operator can tell a deliberate
// backfill from a surprise.
func TestReportMigrationsPrintsNotes(t *testing.T) {
	const note = "migration 2 (0003_seed) contains DML; rows seeded from dev-copy data replay differently on production"
	var err error
	stderr := captureStderr(t, func() {
		err = reportMigrations(&api.MigrationSetResult{
			DatabaseID: "mdb-abc",
			Results:    []api.MigrationResult{{Name: "0003_seed", Outcome: api.MigrationApplied, Seq: 3}},
			Notes:      []string{note},
		})
	})
	if err != nil {
		t.Fatalf("a note must not fail the command: %v", err)
	}
	if !strings.Contains(stderr, note) {
		t.Fatalf("the note must be printed verbatim: %q", stderr)
	}
}

// Only promote reaches an empty result: an empty migrations folder is refused
// before any request.
func TestReportMigrationsEmptyTail(t *testing.T) {
	var err error
	stderr := captureStderr(t, func() {
		err = reportMigrations(&api.MigrationSetResult{
			DatabaseID: "mdb-abc",
			Results:    []api.MigrationResult{},
		})
	})
	if err != nil {
		t.Fatalf("nothing to promote is not a failure: %v", err)
	}
	if !strings.Contains(stderr, "Nothing to do") {
		t.Fatalf("an empty tail must say so rather than print an empty list: %q", stderr)
	}
}

func TestMigrationSetResultFilters(t *testing.T) {
	r := &api.MigrationSetResult{Results: []api.MigrationResult{
		{Name: "a", Outcome: api.MigrationSkipped},
		{Name: "b", Outcome: api.MigrationPending},
		{Name: "c", Outcome: api.MigrationDrifted},
		{Name: "d", Outcome: api.MigrationPending},
	}}
	if got := len(r.Pending()); got != 2 {
		t.Fatalf("Pending() = %d, want 2", got)
	}
	if got := r.Drifted(); len(got) != 1 || got[0].Name != "c" {
		t.Fatalf("Drifted() = %+v, want just c", got)
	}
}

func TestDatabaseCommandWiring(t *testing.T) {
	db := newDatabaseCmd()

	find := func(path ...string) *cobra.Command {
		t.Helper()
		cur := db
		for _, name := range path {
			var next *cobra.Command
			for _, c := range cur.Commands() {
				if c.Name() == name {
					next = c
					break
				}
			}
			if next == nil {
				t.Fatalf("no `db %s` command", strings.Join(path, " "))
			}
			cur = next
		}
		return cur
	}

	for _, path := range [][]string{{"sql"}, {"migrate", "status"}, {"migrate", "push"}} {
		cmd := find(path...)
		flag := cmd.Flags().Lookup("env")
		if flag == nil {
			t.Errorf("`db %s` has no --env flag", strings.Join(path, " "))
			continue
		}
		if flag.DefValue != string(api.EnvironmentProd) {
			t.Errorf("`db %s` --env defaults to %q, want %q",
				strings.Join(path, " "), flag.DefValue, api.EnvironmentProd)
		}
	}

	// A dev-redirected promote would apply the copy into itself and report
	// success.
	promote := find("promote")
	if promote.Flags().Lookup("env") != nil {
		t.Error("`db promote` must not take --env")
	}
	if promote.Flags().Lookup("dry-run") == nil {
		t.Error("`db promote` needs --dry-run")
	}
	if promote.Flags().Lookup("yes") == nil {
		t.Error("`db promote` needs --yes: it is the one `db` verb that changes production's schema")
	}
	if err := promote.Args(promote, nil); err == nil {
		t.Error("`db promote` must require the production database id")
	}
}

func TestConfirmPromoteGate(t *testing.T) {
	if ok, err := confirmPromote("mdb-abc", true, false); !ok || err != nil {
		t.Errorf("--dry-run must not ask: ok=%v err=%v", ok, err)
	}
	// `go test` gives no terminal.
	_, err := confirmPromote("mdb-abc", false, false)
	if err == nil {
		t.Fatal("a real promote with no --yes and no terminal must be refused")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the refusal must name the flag that answers it, got %q", err)
	}
	if ok, err := confirmPromote("mdb-abc", false, true); !ok || err != nil {
		t.Errorf("--yes must confirm: ok=%v err=%v", ok, err)
	}
}

// A float64 round trip silently writes a different bigint, undetectable
// downstream. The server holds the same guard (TestParamsKeepPrecision in
// api/v2/database).
func TestParseSQLParamsKeepsPrecision(t *testing.T) {
	// 2^53+1, the smallest integer a float64 cannot represent, and a real-size
	// bigint id.
	const input = `[9007199254740993, 1234567890123456789]`

	got, err := parseSQLParams(input)
	if err != nil {
		t.Fatalf("parseSQLParams: %v", err)
	}
	want := []string{"9007199254740993", "1234567890123456789"}
	for i, w := range want {
		if string(got[i]) != w {
			t.Errorf("param %d = %s, want %s — a float64 round trip corrupted it", i, got[i], w)
		}
	}

	// Marshalling onto the wire is the second place a float64 round trip can
	// happen.
	body, err := json.Marshal(api.DatabaseSQLInput{SQL: "x", Params: got})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		if !strings.Contains(string(body), w) {
			t.Errorf("request body lost %s: %s", w, body)
		}
	}
}

// The nil case matters most: nil means the server could not say, and must not
// render as "0 rows affected".
func TestReportRowsAffected(t *testing.T) {
	say := func(n *int) string {
		return captureStderr(t, func() { reportRowsAffected(n) })
	}

	forty := 40
	if got := say(&forty); !strings.Contains(got, "40 rows affected") {
		t.Errorf("a write that changed rows must say so: %q", got)
	}

	// Zero is a real answer: the statement ran and matched nothing.
	zero := 0
	if got := say(&zero); !strings.Contains(got, "0 rows affected") {
		t.Errorf("a write that matched nothing must say so rather than stay silent: %q", got)
	}

	one := 1
	if got := say(&one); !strings.Contains(got, "1 row affected") {
		t.Errorf("the singular must not read \"1 rows\": %q", got)
	}

	if got := say(nil); got != "" {
		t.Errorf("with no number from the server the command must say NOTHING, not zero: %q", got)
	}
}

// rowsAffected is the one envelope field where null and 0 differ, so the key is
// always present (no omitempty) and null survives the round trip.
func TestDatabaseSQLResultKeepsRowsAffectedNull(t *testing.T) {
	var res api.DatabaseSQLResult
	if err := json.Unmarshal([]byte(`{"rowCount":3}`), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.RowsAffected != nil {
		t.Errorf("an ABSENT rowsAffected must decode to nil, got %d", *res.RowsAffected)
	}
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"rowsAffected":null`) {
		t.Errorf("--json must always carry the key, null included: %s", body)
	}

	zero := 0
	res.RowsAffected = &zero
	body, err = json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"rowsAffected":0`) {
		t.Errorf("a real 0 must reach the envelope as 0, distinct from null: %s", body)
	}
}
