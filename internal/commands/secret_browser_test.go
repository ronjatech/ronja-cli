package commands

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The `ronja secret create --in-browser` tests (plan §9 C5). The fake answers
// POST /secret/pending once, then a SCRIPT of GET /secret/:id answers. Any other
// request fails the test — above all POST /secret, the route that carries a
// value: this path must never send one.

const (
	pendingID  = "secret-pend-1"
	pendingURL = "https://app.ronja.test/secrets/secret-pend-1?org=tenant-abc"
)

type pendingFake struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	requests []string
	bodies   [][]byte

	// createStatus/createBody replace the success answer.
	createStatus int
	createBody   string
	existing     []map[string]any
	reused       bool

	answers []secretAnswer
	gets    int
}

func newPendingFake(t *testing.T, answers ...secretAnswer) *pendingFake {
	t.Helper()
	f := &pendingFake{t: t, answers: answers}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	signInTo(t, f.server.URL)
	return f
}

func (f *pendingFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.bodies = append(f.bodies, raw)

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/secret/pending":
		if f.createStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.createStatus)
			_, _ = io.WriteString(w, f.createBody)
			return
		}
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		existing := f.existing
		if existing == nil {
			existing = []map[string]any{}
		}
		secretType, _ := in["secretType"].(string)
		writeJSON(w, map[string]any{
			"secret": map[string]any{
				"id": pendingID, "name": in["name"], "secretType": secretType,
				"featureID": in["featureID"], "status": "pending",
			},
			"url":      pendingURL,
			"reused":   f.reused,
			"existing": existing,
		})

	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/secret/"+pendingID:
		if len(f.answers) == 0 {
			f.t.Errorf("GET /secret/:id with no scripted answer left")
			http.Error(w, "no answer", http.StatusInternalServerError)
			return
		}
		a := f.answers[0]
		if len(f.answers) > 1 {
			f.answers = f.answers[1:]
		}
		f.gets++
		if a.httpStatus != 0 {
			writeJSONStatus(w, a.httpStatus, map[string]any{"error": http.StatusText(a.httpStatus)})
			return
		}
		writeJSON(w, map[string]any{"id": pendingID, "name": "Stripe", "status": a.status, "featureID": "collection-1"})

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (f *pendingFake) createBodyJSON(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, req := range f.requests {
		if req == "POST /api/v2/secret/pending" {
			var body map[string]any
			if err := json.Unmarshal(f.bodies[i], &body); err != nil {
				t.Fatalf("pending body is not JSON: %v", err)
			}
			return body
		}
	}
	t.Fatalf("no POST /secret/pending among %v", f.requests)
	return nil
}

func (f *pendingFake) everyBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, b := range f.bodies {
		all = append(all, string(b))
	}
	return strings.Join(all, "\n")
}

func (f *pendingFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func fastInBrowser(t *testing.T, limit time.Duration) {
	t.Helper()
	savedInterval, savedLimit := inBrowserPollInterval, inBrowserWaitLimit
	inBrowserPollInterval = time.Millisecond
	inBrowserWaitLimit = limit
	t.Cleanup(func() { inBrowserPollInterval, inBrowserWaitLimit = savedInterval, savedLimit })
}

// noPrompt fails the test if the echo-off prompt is reached.
func noPrompt(t *testing.T) {
	t.Helper()
	saved := readPassword
	readPassword = func(int) ([]byte, error) {
		t.Fatalf("--in-browser prompted for a value")
		return nil, nil
	}
	t.Cleanup(func() { readPassword = saved })
}

var stripeInBrowser = []string{"secret", "create", "--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com", "--in-browser"}

// C5: pending → ready; the SERVER's url is the one opened; the id is printed;
// a sentinel on stdin is never read, never sent.
func TestSecretInBrowserPendingToReady(t *testing.T) {
	fastInBrowser(t, 5*time.Second)
	opened := fakeBrowser(t)
	noPrompt(t)
	withStdin(t, secretSentinel) // must be left unread
	f := newPendingFake(t,
		secretAnswer{status: "pending"},
		secretAnswer{status: "pending"},
		secretAnswer{status: "ready"},
	)

	stdout, stderr, errText := runSecretCLI(t, stripeInBrowser...)
	if errText != "" {
		t.Fatalf("failed: %s\nstderr: %s", errText, stderr)
	}
	if len(*opened) != 1 || (*opened)[0] != pendingURL {
		t.Fatalf("browser handed %v, want exactly the server's url", *opened)
	}
	want := pendingID + "  Stripe (api_key, feature collection-1, 1 field: apiKey — value entered in Ronja)\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	body := f.createBodyJSON(t)
	if _, ok := body["credentials"]; ok {
		t.Fatalf("the pending request carried credentials: %v", body)
	}
	if body["secretType"] != "api_key" || body["featureID"] != "collection-1" {
		t.Fatalf("body = %v", body)
	}
	if hosts, _ := body["allowedURLs"].([]any); len(hosts) != 1 || hosts[0] != "api.stripe.com" {
		t.Fatalf("allowedURLs = %v", body["allowedURLs"])
	}
	if fields, _ := body["agentFields"].([]any); len(fields) != 1 || fields[0] != "apiKey" {
		t.Fatalf("agentFields = %v", body["agentFields"])
	}
	if strings.Contains(f.everyBody(), secretSentinel) {
		t.Fatal("a value on stdin was sent")
	}
	if rest, _ := io.ReadAll(os.Stdin); string(rest) != secretSentinel {
		t.Fatalf("stdin was read (%d bytes left of %d)", len(rest), len(secretSentinel))
	}
	if strings.Contains(stdout, pendingURL) {
		t.Fatalf("the link reached stdout:\n%s", stdout)
	}
	assertNoSentinel(t, stdout, stderr, errText)
}

// C5: a database login; --json is metadata only.
func TestSecretInBrowserDatabaseJSON(t *testing.T) {
	fastInBrowser(t, 5*time.Second)
	fakeBrowser(t)
	noPrompt(t)
	withTerminalStdin(t)
	f := newPendingFake(t, secretAnswer{status: "ready"})

	stdout, stderr, errText := runSecretCLI(t, "--json", "secret", "create", "--feature", "collection-1", "--name", "Warehouse", "--dialect", "postgres", "--in-browser")
	if errText != "" {
		t.Fatalf("failed: %s\nstderr: %s", errText, stderr)
	}
	body := f.createBodyJSON(t)
	if body["dialect"] != "postgres" || body["secretType"] != "database" || body["fieldSchema"] != nil || body["allowedURLs"] != nil {
		t.Fatalf("body = %v, want a database login with no schema or hosts of the CLI's own", body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("--json stdout does not decode: %v\n%s", err, stdout)
	}
	if out["id"] != pendingID || out["enteredInRonja"] != true || out["dialect"] != "postgres" {
		t.Fatalf("json = %v", out)
	}
	assertNoForbiddenKeys(t, out)
}

// C5: existing[] is printed as a notice on stderr, and the command goes on.
func TestSecretInBrowserPrintsExisting(t *testing.T) {
	fastInBrowser(t, 5*time.Second)
	fakeBrowser(t)
	withTerminalStdin(t)
	f := newPendingFake(t, secretAnswer{status: "ready"})
	f.existing = []map[string]any{{"id": "secret-old", "name": "Stripe (test)", "secretType": "api_key", "status": "ready", "providerKey": "api.stripe.com", "ownedByYou": true}}

	stdout, stderr, errText := runSecretCLI(t, stripeInBrowser...)
	if errText != "" {
		t.Fatalf("failed: %s", errText)
	}
	if !strings.Contains(stderr, "Already in Ronja for the same system") || !strings.Contains(stderr, "secret-old  Stripe (test) (api.stripe.com, ready, yours)") {
		t.Fatalf("stderr does not list the existing secret:\n%s", stderr)
	}
	if strings.Contains(stdout, "secret-old") {
		t.Fatalf("the notice reached stdout:\n%s", stdout)
	}
}

// C5: --no-browser prints the server's link and opens nothing.
func TestSecretInBrowserNoBrowserPrintsTheLink(t *testing.T) {
	fastInBrowser(t, 5*time.Second)
	opened := fakeBrowser(t)
	withTerminalStdin(t)
	newPendingFake(t, secretAnswer{status: "ready"})

	_, stderr, errText := runSecretCLI(t, append(stripeInBrowser, "--no-browser")...)
	if errText != "" {
		t.Fatalf("failed: %s", errText)
	}
	if len(*opened) != 0 {
		t.Fatalf("a browser opened with --no-browser: %v", *opened)
	}
	if !strings.Contains(stderr, "Open this link to enter the value in Ronja") || !strings.Contains(stderr, pendingURL) {
		t.Fatalf("stderr does not carry the link:\n%s", stderr)
	}
}

// C5: refusals before anything is sent.
func TestSecretInBrowserRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.txt")
	if err := os.WriteFile(path, []byte(secretSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"--from-file with --in-browser", append(append([]string{}, stripeInBrowser...), "--from-file", path), "--in-browser reads no value here"},
		{"--no-browser alone", []string{"secret", "create", "--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com", "--no-browser"}, "--no-browser goes with --in-browser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opened := fakeBrowser(t)
			noPrompt(t)
			withTerminalStdin(t)
			f := newPendingFake(t)
			stdout, stderr, errText := runSecretCLI(t, tc.args...)
			if !strings.Contains(errText, tc.want) {
				t.Fatalf("error = %q, want %q", errText, tc.want)
			}
			if f.requestCount() != 0 || len(*opened) != 0 {
				t.Fatalf("a refusal sent %d request(s) / opened %v", f.requestCount(), *opened)
			}
			assertNoSentinel(t, stdout, stderr, errText)
		})
	}
}

// C2/B4 of the plan: empty stdin without --in-browser never starts the browser
// path, and the refusal points at it.
func TestSecretCreateEmptyStdinPointsAtInBrowser(t *testing.T) {
	opened := fakeBrowser(t)
	withStdin(t, "")
	f := newPendingFake(t)
	_, _, errText := runSecretCLI(t, "secret", "create", "--feature", "collection-1", "--name", "Stripe", "--host", "api.stripe.com")
	if !strings.Contains(errText, "or pass --in-browser to enter it yourself in Ronja") {
		t.Fatalf("error = %q", errText)
	}
	if f.requestCount() != 0 || len(*opened) != 0 {
		t.Fatal("empty stdin started the browser path")
	}
}

// C5: the server's refusals.
func TestSecretInBrowserForwardsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"409 names the differing field", 409, `{"error":"a pending secret named \"Stripe\" (secret-prior) already exists in this feature with a different allowedURLs — finish or delete that one, or use another name"}`, "with a different allowedURLs"},
		{"400 is the server's sentence", 400, `{"error":"allowedURLs entry \"*.com\" would let the value go to any site under com, which anyone can register"}`, "anyone can register"},
		{"404 with no API body is an older server", 404, `404 page not found`, inBrowserOlderServer},
		{"404 from the API is the feature", 404, `{"error":"not found"}`, "feature collection-1 was not found"},
		{"scoped login", 403, `{"error":"token scope \"secrets\" does not permit write","code":"insufficient_scope"}`, "needs secrets:write"},
		{"role floor or feature write", 403, `{"error":"forbidden"}`, "you cannot create a secret in this feature: it needs the User role, and on a shared feature an Admin or a maintainer of it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opened := fakeBrowser(t)
			withTerminalStdin(t)
			f := newPendingFake(t)
			f.createStatus, f.createBody = tc.status, tc.body
			stdout, _, errText := runSecretCLI(t, stripeInBrowser...)
			if !strings.Contains(errText, tc.want) {
				t.Fatalf("error = %q, want it to contain %q", errText, tc.want)
			}
			if len(*opened) != 0 || stdout != "" {
				t.Fatalf("a refusal opened %v / printed %q", *opened, stdout)
			}
		})
	}
}

// C5: the wait's limit, a deleted secret, and a re-used pending secret.
func TestSecretInBrowserWaitEnds(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		fastInBrowser(t, 30*time.Millisecond)
		fakeBrowser(t)
		withTerminalStdin(t)
		newPendingFake(t, secretAnswer{status: "pending"})
		_, _, errText := runSecretCLI(t, stripeInBrowser...)
		want := "Secret " + pendingID + " stays pending; finish it at " + pendingURL + " or re-run with the same arguments"
		if !strings.Contains(errText, "still waiting for the value") || !strings.Contains(errText, want) {
			t.Fatalf("error = %q, want the timeout message", errText)
		}
	})
	t.Run("deleted while waiting", func(t *testing.T) {
		fastInBrowser(t, 5*time.Second)
		fakeBrowser(t)
		withTerminalStdin(t)
		newPendingFake(t, secretAnswer{status: "pending"}, secretAnswer{httpStatus: 404})
		_, _, errText := runSecretCLI(t, stripeInBrowser...)
		if !strings.Contains(errText, "the secret was deleted while waiting ("+pendingID+")") {
			t.Fatalf("error = %q", errText)
		}
	})
	t.Run("a transient failure is tolerated", func(t *testing.T) {
		fastInBrowser(t, 5*time.Second)
		withPollKnobs(t, time.Minute, time.Millisecond)
		fakeBrowser(t)
		withTerminalStdin(t)
		newPendingFake(t, secretAnswer{httpStatus: 503}, secretAnswer{status: "ready"})
		if _, _, errText := runSecretCLI(t, stripeInBrowser...); errText != "" {
			t.Fatalf("error = %q", errText)
		}
	})
	t.Run("reused", func(t *testing.T) {
		fastInBrowser(t, 5*time.Second)
		fakeBrowser(t)
		withTerminalStdin(t)
		f := newPendingFake(t, secretAnswer{status: "ready"})
		f.reused = true
		_, stderr, errText := runSecretCLI(t, stripeInBrowser...)
		if errText != "" || !strings.Contains(stderr, "Using the pending secret you started earlier: "+pendingID) {
			t.Fatalf("error %q, stderr:\n%s", errText, stderr)
		}
	})
}

// A browser that cannot start falls back to printing the link.
func TestSecretInBrowserFallsBackToPrinting(t *testing.T) {
	fastInBrowser(t, 5*time.Second)
	saved := openBrowser
	openBrowser = func(string) error { return errors.New("no display") }
	t.Cleanup(func() { openBrowser = saved })
	withTerminalStdin(t)
	newPendingFake(t, secretAnswer{status: "ready"})
	_, stderr, errText := runSecretCLI(t, stripeInBrowser...)
	if errText != "" || !strings.Contains(stderr, pendingURL) {
		t.Fatalf("error %q, stderr:\n%s", errText, stderr)
	}
}
