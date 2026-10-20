package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// http.Client.Timeout is a CEILING no per-request context can raise, so a query
// asking for longer would be silently capped.

func slowQueryServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(QueryResult{Result: "id\n1\n", RowCount: 1})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestQueryTimeoutIsNotCappedByTheClientCeiling(t *testing.T) {
	srv := slowQueryServer(t, 150*time.Millisecond)
	client := &Client{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 40 * time.Millisecond},
	}
	in := QueryInput{SQL: "SELECT 1", Format: QueryFormatCSVMeta}

	if _, err := client.Query(context.Background(), in, 30*time.Millisecond); err == nil {
		t.Fatal("a 30ms deadline did not stop a 150ms query")
	}

	// With the shared client this would die at the 40ms ceiling.
	result, err := client.Query(context.Background(), in, 5*time.Second)
	if err != nil {
		t.Fatalf("a deadline past the client ceiling was capped by it: %v", err)
	}
	if result.RowCount != 1 {
		t.Errorf("rowCount = %d", result.RowCount)
	}
}

// context.WithTimeout(0) is an already-expired context, the opposite of
// `--timeout 0`.
func TestQueryZeroTimeoutMeansNoDeadline(t *testing.T) {
	srv := slowQueryServer(t, 100*time.Millisecond)
	client := &Client{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 30 * time.Millisecond},
	}

	result, err := client.Query(context.Background(),
		QueryInput{SQL: "SELECT 1", Format: QueryFormatCSVMeta}, 0)
	if err != nil {
		t.Fatalf("--timeout 0 was treated as an expired deadline: %v", err)
	}
	if result.RowCount != 1 {
		t.Errorf("rowCount = %d", result.RowCount)
	}
}

func TestQueryStillHonoursTheCallersContext(t *testing.T) {
	srv := slowQueryServer(t, 300*time.Millisecond)
	client := &Client{BaseURL: srv.URL, HTTP: &http.Client{Timeout: 30 * time.Second}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Query(ctx, QueryInput{SQL: "SELECT 1"}, 0); err == nil {
		t.Fatal("a cancelled caller context did not stop the query")
	}
}

// The shared client is reused where it can be, so the connection pool is not
// duplicated per request.
func TestHTTPForReusesTheSharedClientWithinTheCeiling(t *testing.T) {
	client := New("http://example.invalid", "")
	if got := client.httpFor(requestTimeout); got != client.HTTP {
		t.Errorf("a deadline inside the ceiling allocated a new client")
	}
	if got := client.httpFor(clientTimeout); got != client.HTTP {
		t.Errorf("a deadline exactly at the ceiling allocated a new client")
	}
	beyond := client.httpFor(clientTimeout + time.Second)
	if beyond == client.HTTP {
		t.Fatal("a deadline past the ceiling reused the capped client")
	}
	if beyond.Timeout != 0 {
		t.Errorf("the copy still carries a ceiling of %s", beyond.Timeout)
	}
	if beyond.Transport != client.HTTP.Transport {
		t.Errorf("the copy does not share the transport, so it has its own connection pool")
	}
}

func TestQueryAdviceIsAnEmptyArrayWhenAbsent(t *testing.T) {
	srv := slowQueryServer(t, 0)
	client := &Client{BaseURL: srv.URL, HTTP: &http.Client{}}
	result, err := client.Query(context.Background(), QueryInput{SQL: "SELECT 1"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	if got := string(envelope["advice"]); got != "[]" {
		t.Fatalf(`advice encoded as %s, want []`, got)
	}
}

// Nothing on the client may re-encode the server's CSV: a one-column NULL row
// stays `""` (the server's repair, so the row survives any CSV reader).
func TestQueryResultIsTheServersCSVVerbatim(t *testing.T) {
	for _, body := range []QueryResult{
		{Result: "x\n1\n\"\"\n3\n", RowCount: 3},
		{Result: "b,d\ntrue,1.50\n,\n", RowCount: 2},
		{Result: "Success\n", RowCount: 0},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		}))
		client := &Client{BaseURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
		got, err := client.Query(context.Background(), QueryInput{SQL: "SELECT 1", Format: QueryFormatCSVMeta}, 5*time.Second)
		srv.Close()
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if got.Result != body.Result || got.RowCount != body.RowCount {
			t.Fatalf("result = %q (%d rows), want %q (%d)", got.Result, got.RowCount, body.Result, body.RowCount)
		}
	}
}
