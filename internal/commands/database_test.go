package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// TestParseSQLParams covers the --params decode. It is decoded locally so a
// malformed value fails naming the flag, rather than arriving at the server as a
// type error about a field the user never typed.
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
		// Someone will type --params 'a@b.c' expecting one parameter. Sending it
		// as a non-array would be a confusing server-side error, so it fails here.
		if _, err := parseSQLParams(`"a@b.c"`); err == nil {
			t.Fatal("want an error for a scalar — params is an array")
		}
	})
}

// TestReportMigrationsFailsOnDrift is the CI-gate guarantee: `db migrate status`
// exits non-zero when something has drifted, so a pipeline does not have to
// parse its output to notice.
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

// TestReportMigrationsCleanRuns confirms the ordinary paths exit zero.
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

// TestParseEnvFlag covers --env validation. It is local so a typo fails naming
// the flag the person typed, rather than arriving as a server 400 about a query
// parameter they never saw — and it fails BEFORE a migration set is sent.
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
		// "development" is the plausible typo, and the one most likely to be
		// typed by someone who has met another tool. It must not run against
		// production because it was not spelled "dev".
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

// TestReportEnvironmentNamesTheDevCopy is the misreading this line exists to
// prevent: a set applied to the dev copy that reads exactly like one applied to
// production. Its callers print it BEFORE the verdicts, because by the time the
// counts are on screen the reader has decided what they mean.
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

	// And a production run says nothing extra — the note is the exception, not
	// a banner on every report.
	quiet := captureStderr(t, func() {
		reportEnvironment(api.EnvironmentProd, "prod", "mdb-abc", "")
	})
	if quiet != "" {
		t.Errorf("a production run must print nothing: %q", quiet)
	}
}

// TestReportEnvironmentWarnsWhenProductionAnswered covers the one case the
// server-side gate cannot cover: an instance old enough not to know
// ?environment=dev answers from production without complaint, and the command
// otherwise looks like it did exactly what was asked.
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

// TestReportMigrationsSaysNothingAboutTheEnvironment pins the split: the
// environment line has exactly one writer, so it cannot be printed twice (once
// per report and once per command) or omitted in --json mode.
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

// TestReportMigrationsPrintsNotes covers promote's advisory half: the note is
// printed, and it does NOT become a failure. Only the operator can tell a
// deliberate backfill from a surprise, so this is text rather than a refusal.
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

// TestReportMigrationsEmptyTail: an already-promoted database is a clean,
// repeatable no-op, not a failure. Only promote reaches this — an empty
// migrations folder is refused before any request goes out.
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

// TestMigrationSetResultFilters pins the two helpers the command branches on.
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

// TestDatabaseCommandWiring is the wiring half of the two tests above: a
// validator no flag reaches is dead code, and a promote command nobody
// registered is a help page that never renders.
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

	// --env has to exist on all three, defaulting to production. A default of
	// "dev" on any one of them would be a statement silently run somewhere else.
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

	// promote deliberately has NO --env: it names production explicitly, and a
	// dev-redirected promote would apply the copy into itself and report success.
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

// TestConfirmPromoteGate pins the three answers the gate gives. The middle one
// is the one that matters: a promote from CI or an agent, where there is no
// terminal to prompt on, must refuse rather than run unasked.
func TestConfirmPromoteGate(t *testing.T) {
	// A dry run changes nothing, so it never asks.
	if ok, err := confirmPromote("mdb-abc", true, false); !ok || err != nil {
		t.Errorf("--dry-run must not ask: ok=%v err=%v", ok, err)
	}
	// No --yes and no terminal (`go test` gives none) — refused, naming the flag.
	_, err := confirmPromote("mdb-abc", false, false)
	if err == nil {
		t.Fatal("a real promote with no --yes and no terminal must be refused")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the refusal must name the flag that answers it, got %q", err)
	}
	// --yes IS the answer.
	if ok, err := confirmPromote("mdb-abc", false, true); !ok || err != nil {
		t.Errorf("--yes must confirm: ok=%v err=%v", ok, err)
	}
}

// TestParseSQLParamsKeepsPrecision is the guard for a silent data-corruption bug.
//
// Decoding the parameter list into []any turns every JSON number into a float64,
// which cannot represent an integer above 2^53. A caller binding a bigint id —
// a Snowflake id, an epoch in nanoseconds, anything from a system that does not
// use sequential ints — would have had a DIFFERENT number written to their
// database, with no error at any layer. Nothing downstream can detect it,
// because by the time the value reaches the server the original digits are gone.
//
// Keeping the elements as raw JSON is the fix, and it has to hold on BOTH sides:
// the server has the same guard (TestParamsKeepPrecision in api/v2/database).
func TestParseSQLParamsKeepsPrecision(t *testing.T) {
	// 2^53+1 — the smallest integer a float64 cannot represent — and a value
	// well past it, of the size a real bigint id reaches.
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

	// And it must survive being marshalled onto the wire, which is where the
	// second float64 round trip used to happen.
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
