package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReadOnlyGuardRefusesBeforeSending: a guarded context's write never
// reaches the server unless Allow admits it; reads always go through.
func TestReadOnlyGuardRefusesBeforeSending(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	g := &ReadOnlyGuard{Allow: func(method, path string) bool { return method == "PUT" && path == "managed/x/applied" }}
	ctx := WithReadOnlyGuard(context.Background(), g)
	if err := c.Do(ctx, "GET", "workflow/a", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "PUT", "managed/x/applied", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "POST", "workflow/a/checkout", nil, nil); !errors.Is(err, ErrWriteRefused) {
		t.Fatalf("a refused write answered %v", err)
	}
	if want := []string{"GET /api/v2/workflow/a", "PUT /api/v2/managed/x/applied"}; len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("the server saw %v", seen)
	}
	if b := g.Blocked(); len(b) != 1 || b[0] != "POST /api/v2/workflow/a/checkout" {
		t.Fatalf("blocked = %v", b)
	}
	if err := c.Do(context.Background(), "POST", "workflow/a/checkout", nil, nil); err != nil {
		t.Fatalf("an unguarded write was refused: %v", err)
	}
}

// TestReadOnlyGuardCoversDoRaw: DoRaw is the other way a request leaves the
// process, and a guarded context's write through it is refused the same way —
// an /api/v2/ route by Allow, anything else outright.
func TestReadOnlyGuardCoversDoRaw(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	g := &ReadOnlyGuard{Allow: func(method, path string) bool { return method == "PUT" && path == "managed/x/applied" }}
	ctx := WithReadOnlyGuard(context.Background(), g)
	raw := func(method, path string) error {
		resp, err := c.DoRaw(ctx, method, path, nil, nil, 0)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := raw("GET", "/api/v2/workflow/a"); err != nil {
		t.Fatal(err)
	}
	if err := raw("PUT", "/api/v2/managed/x/applied"); err != nil {
		t.Fatal(err)
	}
	if err := raw("POST", "/api/v2/workflow/a/checkout"); !errors.Is(err, ErrWriteRefused) {
		t.Fatalf("a refused write answered %v", err)
	}
	if err := raw("POST", "service-api/v1/email/send"); !errors.Is(err, ErrWriteRefused) {
		t.Fatalf("a write outside /api/v2 answered %v", err)
	}
	if want := []string{"GET /api/v2/workflow/a", "PUT /api/v2/managed/x/applied"}; len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("the server saw %v", seen)
	}
	if b := g.Blocked(); len(b) != 2 || b[0] != "POST /api/v2/workflow/a/checkout" || b[1] != "POST /service-api/v1/email/send" {
		t.Fatalf("blocked = %v", b)
	}
}
