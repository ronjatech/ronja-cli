package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// net/http strips Authorization only on a redirect to a different HOSTNAME, so
// a 302 to another PORT, or an https→http downgrade, would forward the token.

// recordingServer is locked because the redirect chain and the assertion run
// on different goroutines.
type recordingServer struct {
	mu   sync.Mutex
	hits int
	auth []string

	server *httptest.Server
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	r := &recordingServer{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits++
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		writeTestJSON(w, `{"ok":true}`)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *recordingServer) sawAuthorization() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits, append([]string(nil), r.auth...)
}

func writeTestJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestRedirectToAnotherPortIsRefused(t *testing.T) {
	elsewhere := newRecordingServer(t)
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.server.URL+"/api/v2/thing", http.StatusFound)
	}))
	t.Cleanup(instance.Close)

	client := New(instance.URL, "secret-token")
	_, err := client.DoRaw(context.Background(), "GET", "/api/v2/thing", nil, nil, 5*time.Second)
	if err == nil {
		t.Fatal("the redirect was followed")
	}
	if !strings.Contains(err.Error(), "different origin") {
		t.Errorf("error does not say why it stopped: %v", err)
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("error does not say the credential was withheld: %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("the token leaked into the error: %v", err)
	}
	if hits, auth := elsewhere.sawAuthorization(); hits != 0 {
		t.Errorf("the other origin was contacted %d times, carrying %v", hits, auth)
	}
}

func TestSameOriginRedirectIsFollowed(t *testing.T) {
	// The handler runs on the server's goroutines, the assertions on this one.
	var mu sync.Mutex
	var seen []string
	var authorized int
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		if r.Header.Get("Authorization") == "Bearer secret-token" {
			authorized++
		}
		mu.Unlock()
		if r.URL.Path == "/api/v2/thing" {
			http.Redirect(w, r, "/api/v2/thing/", http.StatusFound)
			return
		}
		writeTestJSON(w, `{"ok":true}`)
	}))
	t.Cleanup(instance.Close)

	client := New(instance.URL, "secret-token")
	resp, err := client.DoRaw(context.Background(), "GET", "/api/v2/thing", nil, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("a same-origin redirect was refused: %v", err)
	}
	defer resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("paths served = %v, want the redirect to have been followed", seen)
	}
	if authorized != 2 {
		t.Errorf("the credential reached %d of 2 same-origin requests", authorized)
	}
}

func TestRedirectGuardOrigins(t *testing.T) {
	cases := []struct {
		base, target string
		allowed      bool
	}{
		{"https://app.ronja.tech", "https://app.ronja.tech/x", true},
		{"https://app.ronja.tech:443", "https://app.ronja.tech/x", true},
		{"http://localhost:8080", "http://localhost:8080/x", true},
		{"https://APP.ronja.tech", "https://app.ronja.tech/x", true},
		{"http://localhost:8080", "http://localhost:9999/x", false},
		{"https://app.ronja.tech", "http://app.ronja.tech/x", false},
		{"https://app.ronja.tech", "https://evil.example/x", false},
	}
	for _, c := range cases {
		guard := redirectGuard(c.base)
		req, err := http.NewRequest("GET", c.target, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = guard(req, nil)
		if allowed := err == nil; allowed != c.allowed {
			t.Errorf("%s -> %s: allowed = %v, want %v (%v)", c.base, c.target, allowed, c.allowed, err)
		}
	}
}

// A CheckRedirect REPLACES net/http's default ten-hop limit, so a guard checking
// origins alone would follow a same-origin loop forever.
func TestSameOriginRedirectLoopStops(t *testing.T) {
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v2/thing", http.StatusFound)
	}))
	t.Cleanup(instance.Close)

	client := New(instance.URL, "secret-token")
	_, err := client.DoRaw(context.Background(), "GET", "/api/v2/thing", nil, nil, 5*time.Second)
	if err == nil {
		t.Fatal("a redirect loop was followed to completion, which cannot happen")
	}
	if !strings.Contains(err.Error(), "stopped after") {
		t.Errorf("error does not name the hop limit: %v", err)
	}
}

// httpFor hands a long-deadline request a shallow COPY of the client, which
// must keep CheckRedirect.
func TestHTTPForCopyKeepsTheRedirectGuard(t *testing.T) {
	client := New("http://example.invalid", "")
	copied := client.httpFor(clientTimeout + time.Second)
	if copied == client.HTTP {
		t.Fatal("httpFor did not copy, so this test proves nothing")
	}
	if copied.CheckRedirect == nil {
		t.Fatal("the copy has no redirect guard")
	}
	req, err := http.NewRequest("GET", "http://evil.example/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if copied.CheckRedirect(req, nil) == nil {
		t.Error("the copy's guard allows a cross-origin redirect")
	}
}
