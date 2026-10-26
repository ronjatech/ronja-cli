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

// A request names exactly the database it runs on: no ?environment= rides
// along, so nothing about it depends on a server-side redirect.
func TestDatabaseSQLSendsNoEnvironment(t *testing.T) {
	client, seen, _ := databaseServer(t, 200, `{"databaseID":"mdb-abc","result":"id\n1\n","rowCount":1}`)
	out, err := client.DatabaseSQL(context.Background(), "mdb-abc",
		DatabaseSQLInput{SQL: "SELECT 1"}, time.Minute)
	if err != nil {
		t.Fatalf("DatabaseSQL: %v", err)
	}
	if seen.URL.Path != "/api/v2/database/mdb-abc/sql" {
		t.Errorf("path = %s", seen.URL.Path)
	}
	if seen.URL.RawQuery != "" {
		t.Errorf("query string = %q, want none", seen.URL.RawQuery)
	}
	if out.DatabaseID != "mdb-abc" || out.RowCount != 1 {
		t.Errorf("decoded %+v", out)
	}
}

func TestApplyMigrationsSendsTheSetVerbatim(t *testing.T) {
	client, seen, payload := databaseServer(t, 200, `{
		"databaseID": "mdb-abc",
		"dryRun": false,
		"results": [{"name": "0001_leads", "outcome": "applied", "seq": 1}]
	}`)
	out, err := client.ApplyMigrations(context.Background(), "mdb-abc", MigrationSetInput{
		Migrations: []Migration{{Name: "0001_leads", SQL: "CREATE TABLE leads (id int);\n"}},
	}, time.Minute)
	if err != nil {
		t.Fatalf("ApplyMigrations: %v", err)
	}
	if seen.URL.Path != "/api/v2/database/mdb-abc/migrations" {
		t.Errorf("path = %s", seen.URL.Path)
	}
	if seen.URL.RawQuery != "" {
		t.Errorf("query string = %q, want none", seen.URL.RawQuery)
	}
	if len(out.Results) != 1 || out.Results[0].Outcome != MigrationApplied {
		t.Errorf("decoded %+v", out)
	}

	var body map[string]any
	if err := json.Unmarshal(*payload, &body); err != nil {
		t.Fatal(err)
	}

	// The SQL must arrive verbatim — the ledger hash covers these bytes.
	sent := body["migrations"].([]any)[0].(map[string]any)["sql"]
	if sent != "CREATE TABLE leads (id int);\n" {
		t.Errorf("sql = %q, want the bytes unchanged", sent)
	}
}
