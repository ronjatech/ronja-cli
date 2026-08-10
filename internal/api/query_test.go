package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The property under test is the one that is invisible from the outside:
// http.Client.Timeout is a CEILING no per-request context can raise, so a
// caller asking for longer than it gets silently capped. A query routed to
// Batch compute legitimately runs for minutes, and the failure mode is a
// perfectly healthy query killed with a message about a deadline nobody chose.

// slowQueryServer answers /api/v2/duckdb/query after a delay.
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

// A deadline PAST the client's own ceiling has to actually apply. The ceiling
// here is deliberately tiny so the test is fast; the relationship it pins is
// the same one 120s-vs-`--timeout 10m` has in production.
func TestQueryTimeoutIsNotCappedByTheClientCeiling(t *testing.T) {
	srv := slowQueryServer(t, 150*time.Millisecond)
	client := &Client{
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 40 * time.Millisecond},
	}
	in := QueryInput{SQL: "SELECT 1", Format: QueryFormatCSVMeta}

	// Under the ceiling: still bounded, as it always was.
	if _, err := client.Query(context.Background(), in, 30*time.Millisecond); err == nil {
		t.Fatal("a 30ms deadline did not stop a 150ms query")
	}

	// Past the ceiling: succeeds. This is only possible if the ceiling moved —
	// with the shared client it would have died at 40ms.
	result, err := client.Query(context.Background(), in, 5*time.Second)
	if err != nil {
		t.Fatalf("a deadline past the client ceiling was capped by it: %v", err)
	}
	if result.RowCount != 1 {
		t.Errorf("rowCount = %d", result.RowCount)
	}
}

// Zero means NO deadline, which is `--timeout 0`. Note it cannot be expressed
// by handing 0 to context.WithTimeout — that produces an already-expired
// context, i.e. the exact opposite — so this is worth pinning.
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

// A caller's own cancellation still wins, whatever the request timeout says.
// Removing the client ceiling must not remove the caller's control with it.
func TestQueryStillHonoursTheCallersContext(t *testing.T) {
	srv := slowQueryServer(t, 300*time.Millisecond)
	client := &Client{BaseURL: srv.URL, HTTP: &http.Client{Timeout: 30 * time.Second}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Query(ctx, QueryInput{SQL: "SELECT 1"}, 0); err == nil {
		t.Fatal("a cancelled caller context did not stop the query")
	}
}

// httpFor's three cases, stated directly: the shared client is reused where it
// can be, so the connection pool is not duplicated per request.
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
