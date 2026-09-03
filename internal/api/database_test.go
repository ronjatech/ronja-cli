package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestParseEnvironment pins the accepted set. It is closed on purpose: the
// server refuses anything else, so a value this accepts and that rejects would
// be a flag that passes local validation and then 400s.
func TestParseEnvironment(t *testing.T) {
	ok := map[string]Environment{
		"":       EnvironmentProd,
		"prod":   EnvironmentProd,
		"PROD":   EnvironmentProd,
		"  dev ": EnvironmentDev,
		"dev":    EnvironmentDev,
		"Dev":    EnvironmentDev,
	}
	for in, want := range ok {
		got, err := ParseEnvironment(in)
		if err != nil {
			t.Errorf("ParseEnvironment(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseEnvironment(%q) = %q, want %q", in, got, want)
		}
	}

	// Everything else is a typo, including the plausible ones. "development"
	// and "production" read as correct and are not.
	for _, in := range []string{"prd", "development", "production", "staging", "true"} {
		if _, err := ParseEnvironment(in); err == nil {
			t.Errorf("ParseEnvironment(%q) should be refused", in)
		}
	}
}

// TestWithEnvironmentOnlyForDev is the wire guarantee: asking for production —
// explicitly or by saying nothing — sends the request this CLI has always sent.
//
// It matters beyond tidiness. An instance that predates dev copies knows no
// `environment` parameter, and a byte-identical request is the one that keeps
// working there.
func TestWithEnvironmentOnlyForDev(t *testing.T) {
	const path = "database/mdb-abc/sql"
	if got := withEnvironment(path, EnvironmentProd); got != path {
		t.Errorf("prod added a query string: %q", got)
	}
	if got := withEnvironment(path, ""); got != path {
		t.Errorf("the zero value added a query string: %q", got)
	}
	if got, want := withEnvironment(path, EnvironmentDev), path+"?environment=dev"; got != want {
		t.Errorf("dev = %q, want %q", got, want)
	}

	// A path that already carries a query gets "&", not a second "?". A second
	// "?" makes `environment` part of the previous parameter's value, so the
	// server sees no environment at all — and a request with no environment
	// runs against PRODUCTION.
	const withQuery = "database/mdb-abc/data?table=leads"
	if got, want := withEnvironment(withQuery, EnvironmentDev), withQuery+"&environment=dev"; got != want {
		t.Errorf("dev on a path with a query = %q, want %q", got, want)
	}
}

// databaseServer records what one call actually put on the wire.
func databaseServer(t *testing.T, status int, body string) (*Client, *http.Request, *[]byte) {
	t.Helper()
	var seen http.Request
	var payload []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r
		payload, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "token"), &seen, &payload
}

// TestDatabaseSQLSendsEnvironment covers both halves of the plumbing end to
// end: the query parameter going out, and the two new fields coming back.
func TestDatabaseSQLSendsEnvironment(t *testing.T) {
	t.Run("dev asks for the copy", func(t *testing.T) {
		client, seen, _ := databaseServer(t, 200, `{
			"databaseID": "mdb-dev-1",
			"result": "id\n1\n",
			"rowCount": 1,
			"truncated": false,
			"durationMs": 3,
			"environment": "dev",
			"parentDatabaseID": "mdb-abc"
		}`)
		out, err := client.DatabaseSQL(context.Background(), "mdb-abc",
			DatabaseSQLInput{SQL: "SELECT 1"}, EnvironmentDev, time.Minute)
		if err != nil {
			t.Fatalf("DatabaseSQL: %v", err)
		}
		if seen.URL.Path != "/api/v2/database/mdb-abc/sql" {
			t.Errorf("path = %s", seen.URL.Path)
		}
		if got := seen.URL.Query().Get("environment"); got != "dev" {
			t.Errorf("environment = %q, want dev", got)
		}
		if out.Environment != string(EnvironmentDev) {
			t.Errorf("environment = %q, want the server's own dev label", out.Environment)
		}
		if out.ParentDatabaseID != "mdb-abc" {
			t.Errorf("parentDatabaseID = %q, want the production id", out.ParentDatabaseID)
		}
	})

	t.Run("prod sends no query string at all", func(t *testing.T) {
		client, seen, _ := databaseServer(t, 200, `{"databaseID":"mdb-abc","environment":"prod"}`)
		out, err := client.DatabaseSQL(context.Background(), "mdb-abc",
			DatabaseSQLInput{SQL: "SELECT 1"}, EnvironmentProd, time.Minute)
		if err != nil {
			t.Fatalf("DatabaseSQL: %v", err)
		}
		if seen.URL.RawQuery != "" {
			t.Errorf("prod put %q on the wire; it must be byte-identical to a request with no environment", seen.URL.RawQuery)
		}
		if out.Environment != string(EnvironmentProd) {
			t.Errorf("environment = %q, want prod", out.Environment)
		}
	})
}

// TestApplyMigrationsSendsEnvironment mirrors the SQL case: same parameter, same
// rule, because a migration set is exactly what someone wants to try on a copy
// first.
func TestApplyMigrationsSendsEnvironment(t *testing.T) {
	client, seen, payload := databaseServer(t, 200, `{
		"databaseID": "mdb-dev-1",
		"dryRun": false,
		"results": [{"name": "0001_leads", "outcome": "applied", "seq": 1}],
		"environment": "dev",
		"parentDatabaseID": "mdb-abc"
	}`)
	out, err := client.ApplyMigrations(context.Background(), "mdb-abc", MigrationSetInput{
		Migrations: []Migration{{Name: "0001_leads", SQL: "CREATE TABLE leads (id int);\n"}},
	}, EnvironmentDev, time.Minute)
	if err != nil {
		t.Fatalf("ApplyMigrations: %v", err)
	}
	if seen.URL.Path != "/api/v2/database/mdb-abc/migrations" {
		t.Errorf("path = %s", seen.URL.Path)
	}
	if got := seen.URL.Query().Get("environment"); got != "dev" {
		t.Errorf("environment = %q, want dev", got)
	}
	if out.Environment != string(EnvironmentDev) || out.ParentDatabaseID != "mdb-abc" {
		t.Errorf("dev redirect not reported: %+v", out)
	}

	// The environment must NOT have leaked into the body. It is a query
	// parameter on an admin-gated route precisely so it cannot be set by a JSON
	// key that generated client code or an echoed payload happens to carry.
	var body map[string]any
	if err := json.Unmarshal(*payload, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["environment"]; ok {
		t.Errorf("environment must not be a body field: %s", *payload)
	}

	// And the SQL must arrive verbatim — the ledger hash covers these bytes.
	sent := body["migrations"].([]any)[0].(map[string]any)["sql"]
	if sent != "CREATE TABLE leads (id int);\n" {
		t.Errorf("sql = %q, want the bytes unchanged", sent)
	}
}

// TestPromoteDecodesNotes covers the promote call: the path, the dryRun body,
// and the notes field only this endpoint sends.
func TestPromoteDecodesNotes(t *testing.T) {
	client, seen, payload := databaseServer(t, 200, `{
		"databaseID": "mdb-abc",
		"dryRun": true,
		"results": [{"name": "0003_seed", "outcome": "pending", "seq": 3}],
		"environment": "prod",
		"parentDatabaseID": "",
		"notes": ["migration 1 (0003_seed) contains DML; rows seeded from dev-copy data replay differently on production"]
	}`)
	out, err := client.Promote(context.Background(), "mdb-abc", PromoteInput{DryRun: true}, time.Minute)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if seen.URL.Path != "/api/v2/database/mdb-abc/promote" {
		t.Errorf("path = %s", seen.URL.Path)
	}
	// Promote names production explicitly, so it carries no environment: a
	// redirect would promote the copy into itself and report success.
	if seen.URL.RawQuery != "" {
		t.Errorf("promote must send no environment, got %q", seen.URL.RawQuery)
	}
	if string(*payload) != `{"dryRun":true}` {
		t.Errorf("body = %s, want just dryRun — promote sends no migration list", *payload)
	}
	if len(out.Notes) != 1 {
		t.Fatalf("notes = %v, want the one DML warning", out.Notes)
	}
	if out.Environment != string(EnvironmentProd) {
		t.Errorf("a promote lands on production, got environment %q", out.Environment)
	}
	if len(out.Pending()) != 1 {
		t.Errorf("promote must decode through the same outcome helpers: %+v", out.Results)
	}
}

// TestPromoteDivergenceIsAnOrdinaryError pins what makes `db promote` a usable
// gate: divergence is an HTTP 400 with a written instruction, so it needs no
// second failure mechanism in the command — an ordinary error return already
// carries the server's own words and exits non-zero.
func TestPromoteDivergenceIsAnOrdinaryError(t *testing.T) {
	const message = "the dev copy's history diverged from production at migration 2 (0002_score): refresh the dev copy to rebase, then re-apply your migrations"
	client, _, _ := databaseServer(t, 400, `{"error":`+quote(message)+`}`)

	_, err := client.Promote(context.Background(), "mdb-abc", PromoteInput{}, time.Minute)
	if err == nil {
		t.Fatal("a diverged promote must be an error")
	}
	if StatusOf(err) != 400 {
		t.Errorf("status = %d, want 400", StatusOf(err))
	}
	if CodeOf(err) != message {
		t.Errorf("the server's instruction must survive to the caller, got %q", CodeOf(err))
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
