package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
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
