package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The CLI never mints a Ronja token: POST /api/v2/authentication/token is for a
// person's browser alone. Every api.New client sends through
// http.DefaultTransport, so wrapping it here guards every test in the package.

// mintAttempts counts the refused requests; TestMain fails the run on any.
var mintAttempts atomic.Int64

type mintGuard struct{ base http.RoundTripper }

func (g mintGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/api/v2/authentication/token") {
		mintAttempts.Add(1)
		return nil, errors.New("the CLI must never call POST /api/v2/authentication/token (mint guard, mint_guard_test.go)")
	}
	return g.base.RoundTrip(r)
}

func TestMain(m *testing.M) {
	http.DefaultTransport = mintGuard{base: http.DefaultTransport}
	code := m.Run()
	if n := mintAttempts.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d request(s) to POST /api/v2/authentication/token — the CLI never mints a Ronja token\n", n)
		code = 1
	}
	os.Exit(code)
}

// The negative control: proves the transport swap sits under api.New's client,
// or every other test would pass vacuously. It undoes exactly its own hits,
// never Store(0), which would erase another test's real offence.
func TestMintGuardRefusesAPostToTheMintRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("the mint request reached the server: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(server.Close)
	client := api.New(server.URL, "test-token")

	for _, path := range []string{"authentication/token", "authentication/token/"} {
		before := mintAttempts.Load()
		err := client.Do(context.Background(), http.MethodPost, path, map[string]any{"name": "x"}, nil)
		hits := mintAttempts.Load() - before
		mintAttempts.Add(-hits)
		if err == nil || !strings.Contains(err.Error(), "mint guard") {
			t.Errorf("POST %s: error = %v, want the mint guard's refusal", path, err)
		}
		if hits < 1 {
			t.Errorf("POST %s: the guard counted no hit", path)
		}
	}

	// A GET of the same path, and a POST elsewhere, are not the mint.
	before := mintAttempts.Load()
	_ = client.Do(context.Background(), http.MethodGet, "authentication/token-mine", nil, nil)
	if mintAttempts.Load() != before {
		t.Errorf("the guard counted a request that is not the mint")
	}
}
