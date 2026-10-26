package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
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

// A CI step that adds --json to read the report must not lose the gate: drift
// is non-zero either way, and stdout still holds nothing but the full result.
func TestMigrateStatusFailsOnDriftInBothModes(t *testing.T) {
	drifted := map[string]any{"databaseID": "mdb-abc", "dryRun": true, "results": []map[string]any{
		{"name": "0001_leads", "outcome": "skipped", "seq": 1},
		{"name": "0002_score", "outcome": "drifted", "seq": 2},
	}}
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"human", []string{"db", "migrate", "status", "--database", "mdb-abc"}},
		{"json", []string{"db", "migrate", "status", "--database", "mdb-abc", "--json"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			srv := newDatabaseServer(t)
			srv.migrations = drifted
			signInTo(t, srv.server.URL)

			out, err := runCLI(t, migrationsDir(t), mode.args...)
			if err == nil {
				t.Fatal("drift must exit non-zero")
			}
			if !strings.Contains(err.Error(), "0002_score") {
				t.Fatalf("error must name the drifted migration, got: %v", err)
			}
			if mode.name == "human" {
				if out != "" {
					t.Errorf("the human report belongs on stderr, stdout got %q", out)
				}
				return
			}
			var got api.MigrationSetResult
			if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
				t.Fatalf("stdout must be only the JSON result: %v\n%s", jerr, out)
			}
			if len(got.Results) != 2 || got.Results[1].Name != "0002_score" || got.Results[1].Outcome != api.MigrationDrifted {
				t.Errorf("the JSON must carry the full result, drifted row included: %+v", got.Results)
			}
		})
	}

	t.Run("clean json", func(t *testing.T) {
		srv := newDatabaseServer(t)
		signInTo(t, srv.server.URL)

		out, err := runCLI(t, migrationsDir(t), "db", "migrate", "status", "--database", "mdb-abc", "--json")
		if err != nil {
			t.Fatalf("a clean ledger is not a failure: %v", err)
		}
		var got api.MigrationSetResult
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("stdout must be the JSON result: %v\n%s", jerr, out)
		}
		if len(got.Results) != 1 || got.Results[0].Name != "0001_leads" {
			t.Errorf("results = %+v, want the one skipped migration", got.Results)
		}
	})
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

// A note is advisory, never a failure: only the operator can tell a deliberate
// backfill from a surprise.
func TestReportMigrationsPrintsNotes(t *testing.T) {
	const note = "migration 2 (0003_seed) contains DML; check the rows it writes before pushing it elsewhere"
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

// databaseServer records every request and answers the managed-database
// routes `db sql` and `db migrate` call, plus the organization lookup a
// --database push makes.
type databaseServer struct {
	server   *httptest.Server
	requests []*http.Request
	// migrations is the body the migrations route answers with; set it before
	// the first request to serve a different ledger.
	migrations map[string]any
}

func newDatabaseServer(t *testing.T) *databaseServer {
	t.Helper()
	d := &databaseServer{migrations: map[string]any{"databaseID": "mdb-abc", "results": []map[string]any{
		{"name": "0001_leads", "outcome": "skipped", "seq": 1},
	}}}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.requests = append(d.requests, r.Clone(r.Context()))
		switch r.URL.Path {
		case "/api/v2/authentication/me":
			writeJSON(w, map[string]any{"tenant": map[string]any{"id": "ten-1", "name": "Acme"}})
		case "/api/v2/database/mdb-abc/sql":
			writeJSON(w, map[string]any{"databaseID": "mdb-abc", "result": "id\n1\n", "rowCount": 1})
		case "/api/v2/database/mdb-abc/migrations":
			writeJSON(w, d.migrations)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(d.server.Close)
	return d
}

// migrationsDir is a folder a push would really send, so a refusal that came
// after the request could not pass for one that came before it.
func migrationsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "migrations", "0001_leads.sql"), []byte("CREATE TABLE leads (id int);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const devCopiesRemovedText = "Dev copies of databases were removed. To test without touching live data, use a dev Space: ask an admin to create one, then build and test there against that Space's own database, and build the finished work in your real Space."

// An older script's --env dev or `db promote` meant "not the live database";
// running it anyway would write production, and "unknown flag" would not say
// what to do instead.
func TestRetiredDevCopyCallsAreRefusedBeforeAnyRequest(t *testing.T) {
	for _, args := range [][]string{
		{"db", "sql", "--env", "dev", "mdb-abc", "SELECT 1"},
		{"db", "sql", "--env", "DEV", "mdb-abc", "SELECT 1"},
		{"db", "sql", "--env", "staging", "mdb-abc", "SELECT 1"},
		// No positionals: the refusal must win over the arity error.
		{"db", "sql", "--env", "dev"},
		{"db", "env", "--env", "dev"},
		{"db", "connect", "--env", "dev"},
		{"db", "migrate", "push", "--env", "dev", "extra"},
		{"db", "migrate", "push", "--env", "dev", "--database", "mdb-abc"},
		{"db", "migrate", "status", "--env", "dev", "--database", "mdb-abc"},
		{"db", "env", "--env", "dev", "mdb-abc"},
		{"db", "connect", "--env", "dev", "mdb-abc"},
		{"db", "promote", "mdb-abc"},
		{"db", "promote", "mdb-abc", "--dry-run"},
		{"db", "promote", "mdb-abc", "--yes"},
		{"--json", "db", "promote", "mdb-abc"},
		{"db", "promote", "mdb-abc", "--json"},
		{"db", "promote", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv := newDatabaseServer(t)
			signInTo(t, srv.server.URL)

			_, err := runCLI(t, migrationsDir(t), args...)
			if err == nil {
				t.Fatal("accepted; it must be refused")
			}
			if err.Error() != devCopiesRemovedText {
				t.Errorf("refusal = %q, want the dev-Space sentence", err.Error())
			}
			if len(srv.requests) != 0 {
				t.Errorf("sent %d request(s) before refusing, first %s", len(srv.requests), srv.requests[0].URL)
			}
		})
	}
}

// The flag and the command exist only to refuse; --help must not offer them.
func TestRetiredDevCopySurfaceIsHidden(t *testing.T) {
	db := newDatabaseCmd()
	for _, path := range [][]string{{"sql"}, {"migrate", "status"}, {"migrate", "push"}, {"env"}, {"connect"}} {
		cmd, _, err := db.Find(path)
		if err != nil {
			t.Fatalf("db %s: %v", strings.Join(path, " "), err)
		}
		flag := cmd.Flags().Lookup("env")
		if flag == nil || !flag.Hidden {
			t.Errorf("db %s: --env must exist and be hidden", strings.Join(path, " "))
		}
	}
	promote, _, err := db.Find([]string{"promote"})
	if err != nil || promote.Name() != "promote" {
		t.Fatalf("db promote is gone; it must stay to refuse old scripts: %v", err)
	}
	if !promote.Hidden {
		t.Error("db promote must be hidden")
	}
	// `ronja help db promote` prints Long, so it says why rather than nothing.
	if promote.Long != devCopiesRemovedText {
		t.Errorf("db promote Long = %q, want the refusal sentence", promote.Long)
	}
}

// "prod" always meant the database named, so it and no flag at all both run,
// and neither puts ?environment= on the wire.
func TestDatabaseCommandsRunWithProdOrNoEnv(t *testing.T) {
	for _, args := range [][]string{
		{"db", "sql", "mdb-abc", "SELECT 1"},
		{"db", "sql", "--env", "prod", "mdb-abc", "SELECT 1"},
		{"db", "sql", "--env", " prod ", "mdb-abc", "SELECT 1"},
		// Blank after trimming is the same as no flag.
		{"db", "sql", "--env", " ", "mdb-abc", "SELECT 1"},
		{"db", "migrate", "status", "--database", "mdb-abc"},
		{"db", "migrate", "push", "--database", "mdb-abc"},
		{"db", "migrate", "push", "--env", "prod", "--database", "mdb-abc"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv := newDatabaseServer(t)
			signInTo(t, srv.server.URL)

			if _, err := runCLI(t, migrationsDir(t), args...); err != nil {
				t.Fatalf("refused: %v", err)
			}
			sent := 0
			for _, r := range srv.requests {
				if !strings.HasPrefix(r.URL.Path, "/api/v2/database/") {
					continue
				}
				sent++
				if r.URL.RawQuery != "" {
					t.Errorf("%s carried %q, want no query string", r.URL.Path, r.URL.RawQuery)
				}
			}
			if sent != 1 {
				t.Errorf("sent %d database request(s), want 1", sent)
			}
		})
	}
}
