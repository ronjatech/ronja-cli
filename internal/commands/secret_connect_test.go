package commands

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The `ronja secret connect` tests. The fake answers initiate once, then serves
// a SCRIPT of GET /secret/:id answers — the first is the snapshot, each later one
// a poll — so a test states exactly what the server says, in order, and the
// assertion is when the command stops listening.

const (
	connectSecretID = "secret-google-1"
	connectAuthURL  = "https://accounts.example.test/o/oauth2/auth?state=STATE-SENTINEL-4d1e&client_id=c"
)

// secretAnswer is one GET /secret/:id answer. status 0 means 200 with the
// secret; anything else is that HTTP status with an error body.
type secretAnswer struct {
	httpStatus int
	status     string
	granted    []string
	featureID  string
	updatedAt  string
}

type connectFake struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	requests   []string
	initBodies []map[string]any

	// initiate is the initiate answer: a body, or initStatus+initBody for a
	// refusal. requested nil omits requestedScopes, as an older server does.
	initStatus int
	initBody   string
	requested  []string
	// pageURL, when set, is initiate's `url` — the secret's page in Ronja, as
	// a current server answers. Empty omits it, as an older server does.
	pageURL string

	answers []secretAnswer
	gets    int
	// getTimes is when each GET /secret/:id arrived, for the cadence tests.
	getTimes []time.Time
	// onGet, when set, runs after the n-th GET /secret/:id (1-based) has been
	// answered — outside the lock. It exists for the Ctrl-C test, which must
	// not raise SIGINT until the command is in its wait loop.
	onGet func(n int)
}

func newConnectFake(t *testing.T, requested []string, answers ...secretAnswer) *connectFake {
	t.Helper()
	f := &connectFake{t: t, requested: requested, answers: answers}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	signInTo(t, f.server.URL)
	return f
}

func (f *connectFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)

	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v2/oauth/") && strings.HasSuffix(r.URL.Path, "/initiate"):
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.initBodies = append(f.initBodies, body)
		if f.initStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.initStatus)
			_, _ = io.WriteString(w, f.initBody)
			return
		}
		out := map[string]any{"authURL": connectAuthURL, "secretID": connectSecretID}
		if f.requested != nil {
			out["requestedScopes"] = f.requested
		}
		if f.pageURL != "" {
			out["url"] = f.pageURL
		}
		writeJSON(w, out)

	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/secret/"+connectSecretID:
		if len(f.answers) == 0 {
			f.t.Errorf("GET /secret/:id with no scripted answer left")
			http.Error(w, "no answer", http.StatusInternalServerError)
			return
		}
		f.getTimes = append(f.getTimes, time.Now())
		a := f.answers[0]
		// The last answer repeats, so a wait that never completes keeps
		// getting the same one until its limit.
		if len(f.answers) > 1 {
			f.answers = f.answers[1:]
		}
		f.gets++
		if hook, n := f.onGet, f.gets; hook != nil {
			defer func() { f.mu.Unlock(); hook(n); f.mu.Lock() }()
		}
		if a.httpStatus != 0 {
			writeJSONStatus(w, a.httpStatus, map[string]any{"error": http.StatusText(a.httpStatus)})
			return
		}
		feature := a.featureID
		if feature == "" {
			feature = "feature-google"
		}
		writeJSON(w, map[string]any{
			"id":            connectSecretID,
			"name":          "Google OAuth",
			"secretType":    "oauth",
			"status":        a.status,
			"featureID":     feature,
			"grantedScopes": a.granted,
			"updatedAt":     a.updatedAt,
		})

	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/authentication/token":
		// The CLI never mints a Ronja token.
		f.t.Errorf("connect called POST /authentication/token")
		http.Error(w, "no", http.StatusForbidden)

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (f *connectFake) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

// fastConnect collapses the poll cadence and sets the wait limit.
func fastConnect(t *testing.T, limit time.Duration) {
	t.Helper()
	savedInterval, savedLimit := connectPollInterval, connectWaitLimit
	connectPollInterval = time.Millisecond
	connectWaitLimit = limit
	t.Cleanup(func() { connectPollInterval, connectWaitLimit = savedInterval, savedLimit })
}

// withPollKnobs sets the client's transient-failure window and 429 backoff,
// which the wait reads from the client rather than from package vars.
func withPollKnobs(t *testing.T, window, backoff time.Duration) {
	t.Helper()
	previous := newClient
	newClient = func(baseURL, token string) *api.Client {
		c := previous(baseURL, token)
		c.MaxPollFailureWindow = window
		c.RateLimitBackoff = backoff
		return c
	}
	t.Cleanup(func() { newClient = previous })
}

func (f *connectFake) getTimesCopy() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.getTimes...)
}

// fakeBrowser records every URL a browser would have been handed.
func fakeBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	saved := openBrowser
	openBrowser = func(raw string) error {
		opened = append(opened, raw)
		return nil
	}
	t.Cleanup(func() { openBrowser = saved })
	return &opened
}

// runConnect runs one invocation and checks the properties every run keeps:
// no Ronja token anywhere, and the sign-in link never on stdout.
func runConnect(t *testing.T, args ...string) (stdout, stderr, errText string) {
	t.Helper()
	stdout, stderr, errText = runSecretCLI(t, append([]string{"secret", "connect"}, args...)...)
	for name, s := range map[string]string{"stdout": stdout, "stderr": stderr, "error": errText} {
		if strings.Contains(s, "test-token") {
			t.Fatalf("the Ronja token reached %s:\n%s", name, s)
		}
	}
	if strings.Contains(stdout, "STATE-SENTINEL") || strings.Contains(stdout, connectAuthURL) {
		t.Fatalf("the sign-in link reached stdout:\n%s", stdout)
	}
	return stdout, stderr, errText
}

const (
	gDrive = "https://www.googleapis.com/auth/drive"
	gMail  = "https://www.googleapis.com/auth/gmail.readonly"
)

func TestSecretConnectPendingToReady(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	f := newConnectFake(t, []string{gMail},
		secretAnswer{status: "pending"},
		secretAnswer{status: "pending"},
		secretAnswer{status: "ready", granted: []string{gMail}},
	)

	stdout, stderr, errText := runConnect(t, "gmail")
	if errText != "" {
		t.Fatalf("connect failed: %s\nstderr: %s", errText, stderr)
	}
	if want := "connected: " + connectSecretID + " (Google OAuth, scopes: " + gMail + ")\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if len(*opened) != 1 || (*opened)[0] != connectAuthURL {
		t.Errorf("browser handed %v, want exactly the authURL", *opened)
	}
	if got := f.getCount(); got != 3 {
		t.Errorf("GET /secret/:id %d times, want 3 (snapshot + two polls)", got)
	}
	if body := f.initBodies[0]; body["scopes"] != nil || body["featureID"] != nil {
		t.Errorf("initiate body = %v, want neither scopes nor featureID", body)
	}
}

func TestSecretConnectReauthToReady(t *testing.T) {
	fastConnect(t, 5*time.Second)
	fakeBrowser(t)
	// The grant does not change — only the status does, which is the whole
	// signal from reauth_required.
	newConnectFake(t, []string{gMail},
		secretAnswer{status: "reauth_required", granted: []string{gMail}},
		secretAnswer{status: "reauth_required", granted: []string{gMail}},
		secretAnswer{status: "ready", granted: []string{gMail}},
	)

	stdout, stderr, errText := runConnect(t, "gmail")
	if errText != "" {
		t.Fatalf("connect failed: %s\nstderr: %s", errText, stderr)
	}
	if !strings.HasPrefix(stdout, "connected: ") {
		t.Errorf("stdout = %q, want a connected line", stdout)
	}
}

func TestSecretConnectAlreadyConnectedOpensNothing(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	f := newConnectFake(t, []string{gDrive, gMail},
		secretAnswer{status: "ready", granted: []string{gMail, gDrive}},
	)

	stdout, stderr, errText := runConnect(t, "gmail")
	if errText != "" {
		t.Fatalf("connect failed: %s\nstderr: %s", errText, stderr)
	}
	if want := "already connected: " + connectSecretID + " (Google OAuth, scopes: " + gMail + ", " + gDrive + ")\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if len(*opened) != 0 {
		t.Errorf("a browser opened for a connection that already holds the scopes: %v", *opened)
	}
	if strings.Contains(stderr, "STATE-SENTINEL") {
		t.Errorf("the sign-in link was printed for a connection that needs none:\n%s", stderr)
	}
	if got := f.getCount(); got != 1 {
		t.Errorf("GET /secret/:id %d times, want only the snapshot", got)
	}
}

// TestSecretConnectAccumulateWaitsForTheGrantToChange: a ready Google secret
// asked for one more service. Only a CHANGE in grantedScopes is a consent; the
// fake bumps updatedAt mid-wait, which a background refresh does, and the
// command must keep waiting through it.
func TestSecretConnectAccumulateWaitsForTheGrantToChange(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	f := newConnectFake(t, []string{gDrive, gMail},
		secretAnswer{status: "ready", granted: []string{gDrive}, updatedAt: "2026-10-01T10:00:00Z"},
		secretAnswer{status: "ready", granted: []string{gDrive}, updatedAt: "2026-10-01T10:00:05Z"},
		secretAnswer{status: "ready", granted: []string{gDrive}, updatedAt: "2026-10-01T10:00:09Z"},
		secretAnswer{status: "ready", granted: []string{gDrive, gMail}, updatedAt: "2026-10-01T10:00:12Z"},
	)

	stdout, stderr, errText := runConnect(t, "gmail")
	if errText != "" {
		t.Fatalf("connect failed: %s\nstderr: %s", errText, stderr)
	}
	if len(*opened) != 1 {
		t.Fatalf("browser opened %d times, want once", len(*opened))
	}
	if got := f.getCount(); got != 4 {
		t.Errorf("stopped after %d reads, want 4 — an updatedAt-only change is not a consent", got)
	}
	if want := "connected: " + connectSecretID + " (Google OAuth, scopes: " + gDrive + ", " + gMail + ")\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestSecretConnectNarrowingScopeIsNotPrematureSuccess: --scope replaces a
// broad grant with a narrow one. The broad grant already COVERS the narrow ask,
// which is exactly what must not read as done.
func TestSecretConnectNarrowingScopeIsNotPrematureSuccess(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	broad := []string{"companyinformation", "customer", "invoice"}
	f := newConnectFake(t, []string{"companyinformation", "invoice"},
		secretAnswer{status: "ready", granted: broad},
		secretAnswer{status: "ready", granted: broad, updatedAt: "2026-10-01T10:00:05Z"},
		secretAnswer{status: "ready", granted: []string{"companyinformation", "invoice"}},
	)

	stdout, stderr, errText := runConnect(t, "fortnox", "--scope", "invoice")
	if errText != "" {
		t.Fatalf("connect failed: %s\nstderr: %s", errText, stderr)
	}
	if len(*opened) != 1 {
		t.Errorf("browser opened %d times, want once — a narrowing is not already connected", len(*opened))
	}
	if got := f.getCount(); got != 3 {
		t.Errorf("stopped after %d reads, want 3", got)
	}
	scopes, _ := f.initBodies[0]["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != "invoice" {
		t.Errorf("initiate scopes = %v, want [invoice]", f.initBodies[0]["scopes"])
	}
	if !strings.Contains(stdout, "scopes: companyinformation, invoice") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestSecretConnectPartialGrantExitsOneNamingTheMissing(t *testing.T) {
	fastConnect(t, 5*time.Second)
	fakeBrowser(t)
	newConnectFake(t, []string{gDrive, gMail},
		secretAnswer{status: "pending"},
		secretAnswer{status: "ready", granted: []string{gDrive}},
	)

	stdout, _, errText := runConnect(t, "gmail")
	want := "connected, but Google did not grant: " + gMail + ". gmail calls needing them will fail; re-run and leave them ticked"
	if errText != want {
		t.Errorf("error = %q, want %q", errText, want)
	}
	if !strings.HasPrefix(stdout, "connected: ") {
		t.Errorf("stdout = %q, want the connected line before the failure", stdout)
	}
}

// TestSecretConnectOfflineAccessIsNeverMissing: offline_access asks for a
// refresh token and is never granted back. It must neither block "already
// connected" nor be reported as unticked — the server's ScopesCover rule.
func TestSecretConnectOfflineAccessIsNeverMissing(t *testing.T) {
	t.Run("already connected", func(t *testing.T) {
		fastConnect(t, 5*time.Second)
		opened := fakeBrowser(t)
		newConnectFake(t, []string{"Mail.Read", "offline_access"},
			secretAnswer{status: "ready", granted: []string{"Mail.Read"}},
		)
		stdout, _, errText := runConnect(t, "outlook_mail")
		if errText != "" || !strings.HasPrefix(stdout, "already connected: ") || len(*opened) != 0 {
			t.Errorf("stdout=%q err=%q opened=%v, want already connected with no browser", stdout, errText, *opened)
		}
	})
	t.Run("first connect", func(t *testing.T) {
		fastConnect(t, 5*time.Second)
		fakeBrowser(t)
		newConnectFake(t, []string{"Mail.Read", "offline_access"},
			secretAnswer{status: "pending"},
			secretAnswer{status: "ready", granted: []string{"Mail.Read"}},
		)
		if _, _, errText := runConnect(t, "outlook_mail"); errText != "" {
			t.Errorf("error = %q, want success: offline_access is never granted back", errText)
		}
	})
}

// TestSecretConnectOlderServerNeverShortCircuits: no requestedScopes means the
// CLI cannot know what was asked for, so even a ready secret goes through the
// browser, and the coverage check is skipped with a note.
func TestSecretConnectOlderServerNeverShortCircuits(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	newConnectFake(t, nil,
		secretAnswer{status: "ready", granted: []string{gMail}},
		secretAnswer{status: "ready", granted: []string{gMail, gDrive}},
	)

	stdout, stderr, errText := runConnect(t, "gmail")
	if errText != "" {
		t.Fatalf("connect failed: %s", errText)
	}
	if len(*opened) != 1 {
		t.Errorf("browser opened %d times, want once — an older server never short-circuits", len(*opened))
	}
	if !strings.Contains(stderr, "this server can't confirm the scopes; check the secret in Ronja") {
		t.Errorf("stderr lacks the older-server note:\n%s", stderr)
	}
	if !strings.HasPrefix(stdout, "connected: ") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestSecretConnectTimeoutPrintsTheRecordedGrant(t *testing.T) {
	fastConnect(t, 30*time.Millisecond)
	fakeBrowser(t)
	newConnectFake(t, []string{gDrive, gMail},
		secretAnswer{status: "ready", granted: []string{gDrive}},
	)

	_, _, errText := runConnect(t, "gmail")
	if !strings.Contains(errText, "the sign-in did not complete within") ||
		!strings.Contains(errText, "(the link has expired), or the consent finished without granting anything new: the recorded grant is unchanged. Currently recorded grant: "+gDrive+". Re-run to get a new link") {
		t.Errorf("error = %q, want the timeout message naming both readings and the recorded grant", errText)
	}
}

// From pending, completion is the status reaching ready, so a timeout there
// can only be an unfinished sign-in: the "granted nothing new" reading — which
// exists only for a connect that started from ready — is not offered.
func TestSecretConnectTimeoutFromPendingSaysOnlyExpired(t *testing.T) {
	fastConnect(t, 30*time.Millisecond)
	fakeBrowser(t)
	newConnectFake(t, []string{gMail}, secretAnswer{status: "pending"})

	_, _, errText := runConnect(t, "gmail")
	if !strings.Contains(errText, "(the link has expired). Currently recorded grant: none.") {
		t.Errorf("error = %q, want only the expired reading", errText)
	}
	if strings.Contains(errText, "without granting anything new") {
		t.Errorf("error = %q offers the from-ready reading for a pending secret", errText)
	}
}

func TestSecretConnectTimeoutWithNoGrantSaysNone(t *testing.T) {
	fastConnect(t, 30*time.Millisecond)
	fakeBrowser(t)
	newConnectFake(t, []string{gMail}, secretAnswer{status: "pending"})

	_, _, errText := runConnect(t, "gmail")
	if !strings.Contains(errText, "Currently recorded grant: none.") {
		t.Errorf("error = %q, want 'none'", errText)
	}
}

func TestSecretConnectDeletedWhileWaiting(t *testing.T) {
	fastConnect(t, 5*time.Second)
	fakeBrowser(t)
	f := newConnectFake(t, []string{gMail},
		secretAnswer{status: "pending"},
		secretAnswer{status: "pending"},
		secretAnswer{httpStatus: http.StatusNotFound},
		secretAnswer{status: "ready", granted: []string{gMail}},
	)

	_, _, errText := runConnect(t, "gmail")
	if !strings.Contains(errText, "the secret was deleted while waiting") {
		t.Errorf("error = %q, want the deleted message", errText)
	}
	if got := f.getCount(); got != 3 {
		t.Errorf("read %d times, want 3 — a 404 is terminal, not retried", got)
	}
}

func TestSecretConnectToleratesATransientFailure(t *testing.T) {
	fastConnect(t, 5*time.Second)
	fakeBrowser(t)
	newConnectFake(t, []string{gMail},
		secretAnswer{status: "pending"},
		secretAnswer{httpStatus: http.StatusServiceUnavailable},
		secretAnswer{status: "ready", granted: []string{gMail}},
	)

	if _, _, errText := runConnect(t, "gmail"); errText != "" {
		t.Errorf("a 503 mid-wait failed the connect: %s", errText)
	}
}

func TestSecretConnectScopedTokenForbidden(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	f := newConnectFake(t, []string{gMail})
	f.initStatus = http.StatusForbidden
	f.initBody = `{"error":"token scope \"secrets\" does not permit write","code":"insufficient_scope"}`

	_, _, errText := runConnect(t, "gmail")
	if errText != "this login's token cannot write secrets (needs secrets:write)" {
		t.Errorf("error = %q", errText)
	}
	if len(*opened) != 0 {
		t.Errorf("a browser opened after a refused initiate")
	}
}

func TestSecretConnectUnknownServiceForwardsTheBody(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	f := newConnectFake(t, []string{gMail})
	f.initStatus = http.StatusBadRequest
	f.initBody = `{"error":"unknown service: gmial (one of: fortnox, gmail, hubspot)"}`

	_, _, errText := runConnect(t, "gmial")
	if !strings.Contains(errText, "unknown service: gmial (one of: fortnox, gmail, hubspot)") {
		t.Errorf("error = %q, want the server's list of services", errText)
	}
	if len(*opened) != 0 {
		t.Errorf("a browser opened for an unknown service")
	}
}

// TestSecretConnectFeatureMismatchIsSaid: --feature is ignored for a connection
// that already exists, and the person is told where it really lives.
func TestSecretConnectFeatureMismatchIsSaid(t *testing.T) {
	fastConnect(t, 5*time.Second)
	fakeBrowser(t)
	f := newConnectFake(t, []string{gMail},
		secretAnswer{status: "ready", granted: []string{gMail}, featureID: "feature-other"},
	)

	stdout, stderr, errText := runConnect(t, "gmail", "--feature", "feature-mine")
	if errText != "" {
		t.Fatalf("connect failed: %s", errText)
	}
	if !strings.Contains(stderr, "Google OAuth already lives in feature feature-other; using it.") {
		t.Errorf("stderr lacks the feature notice:\n%s", stderr)
	}
	if f.initBodies[0]["featureID"] != "feature-mine" {
		t.Errorf("initiate featureID = %v, want feature-mine", f.initBodies[0]["featureID"])
	}
	if !strings.HasPrefix(stdout, "already connected: ") {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestSecretConnectJSONIsMetadataOnly: --json stdout decodes, carries the
// metadata, and never the sign-in link or anything named like a credential.
func TestSecretConnectJSONIsMetadataOnly(t *testing.T) {
	fastConnect(t, 5*time.Second)
	fakeBrowser(t)
	newConnectFake(t, []string{gDrive, gMail},
		secretAnswer{status: "pending"},
		secretAnswer{status: "ready", granted: []string{gDrive}},
	)

	stdout, _, errText := runConnect(t, "--json", "gmail")
	if errText == "" {
		t.Errorf("a partial grant succeeded under --json")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	for _, k := range []string{"secretID", "name", "status", "grantedScopes", "missingScopes", "featureID", "alreadyConnected"} {
		if _, ok := got[k]; !ok {
			t.Errorf("--json lacks %q: %v", k, got)
		}
	}
	for k := range got {
		if k == "authURL" || k == "token" || k == "credentials" || k == "value" {
			t.Errorf("--json carries %q", k)
		}
	}
	if missing, _ := got["missingScopes"].([]any); len(missing) != 1 || missing[0] != gMail {
		t.Errorf("missingScopes = %v, want [%s]", got["missingScopes"], gMail)
	}
}

// TestSecretConnectCoveringGrantOpensNothing: without --scope the grant
// accumulates, so a ready grant that COVERS the request is already connected —
// here a held full Drive scope covers the ".readonly" variant the service also
// asks for. Under set equality this opened the browser, and a consent that
// changes nothing would then wait out the whole link lifetime.
func TestSecretConnectCoveringGrantOpensNothing(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	f := newConnectFake(t, []string{gDrive, gDrive + ".readonly"},
		secretAnswer{status: "ready", granted: []string{gDrive}},
	)

	stdout, stderr, errText := runConnect(t, "google_drive")
	if errText != "" {
		t.Fatalf("connect failed: %s\nstderr: %s", errText, stderr)
	}
	if !strings.HasPrefix(stdout, "already connected: ") {
		t.Errorf("stdout = %q, want already connected", stdout)
	}
	if len(*opened) != 0 {
		t.Errorf("a browser opened for a grant that covers the request: %v", *opened)
	}
	if got := f.getCount(); got != 1 {
		t.Errorf("GET /secret/:id %d times, want only the snapshot", got)
	}
}

// TestSecretConnectStaleStoredScopeOpensNothing: for a selectable service the
// request is the required set plus the stored scopes still in its catalog, so a
// stored scope the catalog dropped is NOT in it. The grant still covers the
// request, and a consent could only remove the stale scope — no browser.
func TestSecretConnectStaleStoredScopeOpensNothing(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	newConnectFake(t, []string{"invoice"},
		secretAnswer{status: "ready", granted: []string{"invoice", "retired.scope"}},
	)

	stdout, _, errText := runConnect(t, "fortnox")
	if errText != "" || !strings.HasPrefix(stdout, "already connected: ") || len(*opened) != 0 {
		t.Errorf("stdout=%q err=%q opened=%v, want already connected with no browser", stdout, errText, *opened)
	}
}

// TestSecretConnectWithScopeKeepsEquality: --scope replaces the grant, so a
// broader ready grant is not "already connected" even though it covers the ask.
func TestSecretConnectWithScopeKeepsEquality(t *testing.T) {
	if alreadyConnected([]string{"invoice", "customer"}, []string{"invoice"}, true) {
		t.Error("--scope: a broader grant read as already connected")
	}
	if !alreadyConnected([]string{"invoice"}, []string{"invoice", "offline_access"}, true) {
		t.Error("--scope: an equal grant (offline_access aside) did not read as already connected")
	}
	if !alreadyConnected([]string{"invoice", "customer"}, []string{"invoice"}, false) {
		t.Error("no --scope: a covering grant did not read as already connected")
	}
	if alreadyConnected([]string{"invoice"}, []string{"invoice", "customer"}, false) {
		t.Error("no --scope: a grant missing a scope read as already connected")
	}
}

// TestSecretConnectJSONSaysAlreadyConnected: --json tells already-connected
// from a fresh connect.
func TestSecretConnectJSONSaysAlreadyConnected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []secretAnswer
		want    bool
	}{
		{"already", []secretAnswer{{status: "ready", granted: []string{gMail}}}, true},
		{"fresh", []secretAnswer{{status: "pending"}, {status: "ready", granted: []string{gMail}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastConnect(t, 5*time.Second)
			fakeBrowser(t)
			newConnectFake(t, []string{gMail}, tc.answers...)
			stdout, _, errText := runConnect(t, "--json", "gmail")
			if errText != "" {
				t.Fatalf("connect failed: %s", errText)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
			}
			if got["alreadyConnected"] != tc.want {
				t.Errorf("alreadyConnected = %v, want %v", got["alreadyConnected"], tc.want)
			}
		})
	}
}

// TestSecretConnectCtrlCMidWaitStops: Ctrl-C in the wait loop stops at once and
// says the link stays live.
func TestSecretConnectCtrlCMidWaitStops(t *testing.T) {
	fastConnect(t, 10*time.Second)
	fakeBrowser(t)
	f := newConnectFake(t, []string{gMail}, secretAnswer{status: "pending"})
	var once sync.Once
	f.onGet = func(n int) {
		// n == 2 is the first poll: the wait loop has been entered.
		if n >= 2 {
			once.Do(func() {
				if self, err := os.FindProcess(os.Getpid()); err == nil {
					_ = self.Signal(os.Interrupt)
				}
			})
		}
	}

	start := time.Now()
	stdout, _, errText := runConnect(t, "gmail")
	if !strings.Contains(errText, "cancelled; the sign-in link stays valid until it expires") {
		t.Fatalf("error = %q, want the cancellation", errText)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("Ctrl-C took %s to land", waited)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing after a cancel", stdout)
	}
}

// TestSecretConnectSnapshotNotFound: the secret initiate named is gone before
// the first read — nothing opens and the person is told so.
func TestSecretConnectSnapshotNotFound(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	f := newConnectFake(t, []string{gMail}, secretAnswer{httpStatus: http.StatusNotFound})

	_, _, errText := runConnect(t, "gmail")
	if want := "secret " + connectSecretID + " was deleted before the sign-in started"; errText != want {
		t.Errorf("error = %q, want %q", errText, want)
	}
	if len(*opened) != 0 {
		t.Errorf("a browser opened for a deleted secret")
	}
	if got := f.getCount(); got != 1 {
		t.Errorf("read %d times, want 1", got)
	}
}

// TestSecretConnectPrintsTheLinkWhenNoBrowser: --no-browser never calls the
// browser, and a browser that fails to start falls back to the same wording.
// Either way the link is on stderr only (runConnect checks stdout).
func TestSecretConnectPrintsTheLinkWhenNoBrowser(t *testing.T) {
	const wantLink = "Open this URL to sign in to Google:\n  " + connectAuthURL + "\n"
	t.Run("--no-browser", func(t *testing.T) {
		fastConnect(t, 5*time.Second)
		opened := fakeBrowser(t)
		newConnectFake(t, []string{gMail}, secretAnswer{status: "pending"}, secretAnswer{status: "ready", granted: []string{gMail}})
		_, stderr, errText := runConnect(t, "gmail", "--no-browser")
		if errText != "" {
			t.Fatalf("connect failed: %s", errText)
		}
		if len(*opened) != 0 {
			t.Errorf("--no-browser opened a browser: %v", *opened)
		}
		if !strings.Contains(stderr, wantLink) || strings.Contains(stderr, "Opening the") {
			t.Errorf("stderr = %q, want the link-only wording", stderr)
		}
	})
	t.Run("browser fails to start", func(t *testing.T) {
		fastConnect(t, 5*time.Second)
		saved := openBrowser
		calls := 0
		openBrowser = func(string) error { calls++; return errors.New("no display") }
		t.Cleanup(func() { openBrowser = saved })
		newConnectFake(t, []string{gMail}, secretAnswer{status: "pending"}, secretAnswer{status: "ready", granted: []string{gMail}})
		_, stderr, errText := runConnect(t, "gmail")
		if errText != "" {
			t.Fatalf("connect failed: %s", errText)
		}
		if calls != 1 {
			t.Errorf("openBrowser called %d times, want 1", calls)
		}
		if !strings.Contains(stderr, wantLink) || strings.Contains(stderr, "Opening the") {
			t.Errorf("stderr = %q, want the link-only wording", stderr)
		}
	})
}

// TestSecretConnectRateLimitBackoffResets: a 429 adds the backoff to the next
// wait, and the first success puts the cadence back. Without the reset every
// later poll would wait the backoff too.
func TestSecretConnectRateLimitBackoffResets(t *testing.T) {
	fastConnect(t, 10*time.Second)
	fakeBrowser(t)
	const backoff = 150 * time.Millisecond
	withPollKnobs(t, 10*time.Second, backoff)
	f := newConnectFake(t, []string{gMail},
		secretAnswer{status: "pending"},                      // snapshot
		secretAnswer{httpStatus: http.StatusTooManyRequests}, // poll 1
		secretAnswer{status: "pending"},                      // poll 2: after the backoff
		secretAnswer{status: "pending"},                      // poll 3
		secretAnswer{status: "pending"},                      // poll 4
		secretAnswer{status: "ready", granted: []string{gMail}},
	)

	if _, _, errText := runConnect(t, "gmail"); errText != "" {
		t.Fatalf("connect failed: %s", errText)
	}
	times := f.getTimesCopy()
	if len(times) != 6 {
		t.Fatalf("read %d times, want 6", len(times))
	}
	if gap := times[2].Sub(times[1]); gap < backoff {
		t.Errorf("poll after the 429 came %s later, want at least the backoff %s", gap, backoff)
	}
	for i := 3; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap >= backoff {
			t.Errorf("poll %d came %s after the last — the backoff outlived the 429", i, gap)
		}
	}
}

// TestSecretConnectFailureWindowResetsOnSuccess: the transient window bounds an
// UNBROKEN run of failures. Two blips separated by a success, further apart than
// the window, must not add up to a give-up.
func TestSecretConnectFailureWindowResetsOnSuccess(t *testing.T) {
	fastConnect(t, 10*time.Second)
	fakeBrowser(t)
	withPollKnobs(t, 40*time.Millisecond, time.Millisecond)
	f := newConnectFake(t, []string{gMail},
		secretAnswer{status: "pending"},
		secretAnswer{httpStatus: http.StatusServiceUnavailable},
		secretAnswer{status: "pending"},
		secretAnswer{httpStatus: http.StatusServiceUnavailable},
		secretAnswer{status: "ready", granted: []string{gMail}},
	)
	// Hold the success between the blips past the window.
	f.onGet = func(n int) {
		if n == 3 {
			time.Sleep(80 * time.Millisecond)
		}
	}

	if _, _, errText := runConnect(t, "gmail"); errText != "" {
		t.Errorf("two separated blips gave up: %s", errText)
	}
}

// TestSecretConnectGivesUpAfterTheFailureWindow: an unbroken run of transient
// failures longer than the window ends the wait, naming it.
func TestSecretConnectGivesUpAfterTheFailureWindow(t *testing.T) {
	fastConnect(t, 10*time.Second)
	fakeBrowser(t)
	withPollKnobs(t, 40*time.Millisecond, time.Millisecond)
	newConnectFake(t, []string{gMail},
		secretAnswer{status: "pending"},
		secretAnswer{httpStatus: http.StatusServiceUnavailable},
	)

	start := time.Now()
	_, _, errText := runConnect(t, "gmail")
	if !strings.Contains(errText, "of consecutive failures") {
		t.Errorf("error = %q, want the give-up", errText)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("gave up after %s, want roughly the window", waited)
	}
}

// A current server answers initiate with the secret's page in Ronja. That page
// is what opens — never the provider's sign-in, which only starts when the
// person presses Continue on it.
func TestSecretConnectOpensRonjasPage(t *testing.T) {
	fastConnect(t, 5*time.Second)
	opened := fakeBrowser(t)
	f := newConnectFake(t, []string{gMail},
		secretAnswer{status: "pending"},
		secretAnswer{status: "ready", granted: []string{gMail}},
	)
	const page = "https://app.ronja.test/secrets/" + connectSecretID + "?connect=gmail&org=tenant-a"
	f.pageURL = page

	stdout, stderr, errText := runConnect(t, "gmail")
	if errText != "" {
		t.Fatalf("connect failed: %s\nstderr: %s", errText, stderr)
	}
	if len(*opened) != 1 || (*opened)[0] != page {
		t.Errorf("browser handed %v, want exactly Ronja's page", *opened)
	}
	if strings.Contains(stderr, "STATE-SENTINEL") {
		t.Errorf("the provider's sign-in link was printed although Ronja's page was opened:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Opening Ronja so you can connect Google there.") {
		t.Errorf("stderr does not say Ronja opened:\n%s", stderr)
	}
	if want := "connected: " + connectSecretID + " (Google OAuth, scopes: " + gMail + ")\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}
