package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/useragent"
)

// Every request path this package builds carries the one User-Agent.
func TestEveryRequestPathSendsTheVersionedUserAgent(t *testing.T) {
	prev := UserAgent
	UserAgent = useragent.For("v9.9.9")
	t.Cleanup(func() { UserAgent = prev })

	var seen []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.UserAgent())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	if err := c.Do(ctx, "GET", "thing", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	raw, err := c.DoRaw(ctx, "GET", "/api/v2/raw", nil, nil, 0)
	if err != nil {
		t.Fatalf("DoRaw: %v", err)
	}
	raw.Body.Close()
	if _, err := c.GetText(ctx, "/llms.txt"); err != nil {
		t.Fatalf("GetText: %v", err)
	}
	want := []string{
		"/api/v2/thing ronja-cli/v9.9.9",
		"/api/v2/raw ronja-cli/v9.9.9",
		"/llms.txt ronja-cli/v9.9.9",
	}
	if len(seen) != len(want) {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, seen[i], want[i])
		}
	}
}
