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

// The CLI never mints a Ronja token (plan C7). POST /api/v2/authentication/token
// is for a person's browser alone, and no command — today's or a future one —
// may call it, whatever a fake would answer.
//
// Enforced for the WHOLE package rather than per fake, because a per-fake check
// only covers the commands whose tests happen to use that fake: every client
// this package builds (api.New) sends through http.DefaultTransport, so wrapping
// it here sees every request every test makes. A hit fails the request and the
// whole test binary.

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

// The guard's negative control: a POST to the mint route, sent the way every
// command sends — through a client from api.New — is refused before it leaves
// the process, with and without a trailing slash. This proves the
// http.DefaultTransport swap above really sits under api.New's client; a guard
// that did not would let every other test in the package pass vacuously.
//
// Each refusal counts as a hit, so the test undoes exactly its own hits (Add
// with the negative of what it caused, never Store(0), which would also erase a
// real offender's hit from another test). The TestMain exit branch itself is
// not negatively testable without running the test binary in a subprocess.
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
