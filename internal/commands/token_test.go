package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The `ronja token create` tests (plan C4). The fake answers token-link once,
// then serves a SCRIPT of token-mine answers — the first is the snapshot taken
// before the browser opens, each later one a poll — and every token row carries
// a `token` sentinel, so the assertion that the credential reaches neither
// stream is made on every run.

const (
	// tokenLinkURL is what the fake's token-link answers. Deliberately a
	// different origin from the fake and from anything the CLI could build: a
	// browser handed exactly this string got the server's link verbatim.
	tokenLinkURL   = "https://app.example.test/account?new=1&name=etl&scope=data%3Aread&org=ten-test#tokens"
	tokenSentinel  = "TOKEN-SENTINEL-9f3c"
	tokenOldID     = "api_token-old"
	tokenNewID     = "api_token-new"
	tokenAdminOnly = "Only an Admin can create a personal access token."
)

// mineAnswer is one GET /authentication/token-mine answer: httpStatus 0 is 200
// with tokens.
type mineAnswer struct {
	httpStatus int
	tokens     []map[string]any
}

type tokenFake struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// events is every request, plus "OPEN <url>" when the browser is handed
	// one, in order — so a test can see the snapshot came before the open.
	events      []string
	linkQueries []url.Values

	linkStatus int
	linkBody   string

	answers []mineAnswer
	gets    int
	onGet   func(n int)
}

func newTokenFake(t *testing.T, answers ...mineAnswer) *tokenFake {
	t.Helper()
	f := &tokenFake{t: t, answers: answers}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	signInTo(t, f.server.URL)
	return f
}

func (f *tokenFake) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, r.Method+" "+r.URL.Path)

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/authentication/token-link":
		f.linkQueries = append(f.linkQueries, r.URL.Query())
		if f.linkStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.linkStatus)
			_, _ = io.WriteString(w, f.linkBody)
			return
		}
		writeJSON(w, map[string]any{"url": tokenLinkURL})

	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/authentication/token-mine":
		if len(f.answers) == 0 {
			f.t.Errorf("GET /token-mine with no scripted answer left")
			http.Error(w, "no answer", http.StatusInternalServerError)
			return
		}
		a := f.answers[0]
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
		writeJSON(w, a.tokens)

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (f *tokenFake) eventsCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// tokenBrowser records the URLs a browser was handed, into the fake's event
// log as well.
func tokenBrowser(t *testing.T, f *tokenFake) *[]string {
	t.Helper()
	var opened []string
	saved := openBrowser
	openBrowser = func(raw string) error {
		opened = append(opened, raw)
		if f != nil {
			f.mu.Lock()
			f.events = append(f.events, "OPEN "+raw)
			f.mu.Unlock()
		}
		return nil
	}
	t.Cleanup(func() { openBrowser = saved })
	return &opened
}

func fastToken(t *testing.T, limit time.Duration) {
	t.Helper()
	savedInterval, savedLimit := tokenPollInterval, tokenWaitLimit
	tokenPollInterval = time.Millisecond
	tokenWaitLimit = limit
	t.Cleanup(func() { tokenPollInterval, tokenWaitLimit = savedInterval, savedLimit })
}

func tok(id, name string, grants map[string]string, expiresAt any) map[string]any {
	return map[string]any{
		"id":          id,
		"name":        name,
		"token":       tokenSentinel,
		"kind":        "pat",
		"scopeGrants": grants,
		"expiresAt":   expiresAt,
	}
}

// runToken runs one invocation and checks what every run keeps: neither the
// login's token nor a minted one reaches any stream.
func runToken(t *testing.T, args ...string) (stdout, stderr, errText string) {
	t.Helper()
	stdout, stderr, errText = runSecretCLI(t, append([]string{"token", "create"}, args...)...)
	for name, s := range map[string]string{"stdout": stdout, "stderr": stderr, "error": errText} {
		if strings.Contains(s, tokenSentinel) {
			t.Fatalf("a minted token reached %s:\n%s", name, s)
		}
		if strings.Contains(s, "test-token") {
			t.Fatalf("the login's token reached %s:\n%s", name, s)
		}
	}
	return stdout, stderr, errText
}

var oldToken = tok(tokenOldID, "ronja-cli on laptop", nil, nil)

// The happy path: the server's link is opened verbatim AFTER the snapshot, and
// the token reported is the one whose id was not in it — even though the
// person renamed it on the form.
func TestTokenCreateReportsTheTokenNotInTheSnapshot(t *testing.T) {
	fastToken(t, 5*time.Second)
	f := newTokenFake(t,
		mineAnswer{tokens: []map[string]any{oldToken}},
		mineAnswer{tokens: []map[string]any{oldToken}},
		mineAnswer{tokens: []map[string]any{oldToken, tok(tokenNewID, "Renamed on the form", map[string]string{"structure": "write", "data": "read"}, nil)}},
	)
	opened := tokenBrowser(t, f)

	stdout, stderr, errText := runToken(t, "--name", "etl", "--scope", "data:read", "--scope", "structure:write")
	if errText != "" {
		t.Fatalf("token create failed: %s\nstderr: %s", errText, stderr)
	}
	if want := "created: " + tokenNewID + "  Renamed on the form  data:read, structure:write  expires never\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if len(*opened) != 1 || (*opened)[0] != tokenLinkURL {
		t.Errorf("browser handed %v, want exactly the server's link", *opened)
	}
	ev := f.eventsCopy()
	if len(ev) < 3 || ev[0] != "GET /api/v2/authentication/token-link" || ev[1] != "GET /api/v2/authentication/token-mine" || ev[2] != "OPEN "+tokenLinkURL {
		t.Errorf("events = %v, want token-link, then the snapshot, then the browser", ev)
	}
	q := f.linkQueries[0]
	if q.Get("name") != "etl" || strings.Join(q["scope"], ",") != "data:read,structure:write" {
		t.Errorf("token-link query = %v", q)
	}
	if _, ok := q["expires"]; ok {
		t.Errorf("token-link query carries expires %q; the default is never, which sends none", q.Get("expires"))
	}
	if !strings.Contains(stderr, "It is never sent to this terminal.") {
		t.Errorf("stderr does not say where the token is:\n%s", stderr)
	}
}

func TestTokenCreateExpiresFlag(t *testing.T) {
	saved := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = saved })

	cases := []struct {
		flag, wantParam string
	}{
		{"never", ""},
		{"NEVER", ""},
		{"90d", "90d"},
		{"2026-12-30", "2026-12-30"},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			fastToken(t, 5*time.Second)
			f := newTokenFake(t,
				mineAnswer{tokens: []map[string]any{}},
				mineAnswer{tokens: []map[string]any{tok(tokenNewID, "etl", map[string]string{"data": "read"}, "2026-12-30T00:00:00Z")}},
			)
			tokenBrowser(t, f)
			stdout, _, errText := runToken(t, "--name", "etl", "--scope", "data:read", "--expires", tc.flag)
			if errText != "" {
				t.Fatalf("failed: %s", errText)
			}
			got, present := f.linkQueries[0]["expires"]
			if tc.wantParam == "" && present {
				t.Errorf("--expires %s sent expires=%v; never sends none", tc.flag, got)
			}
			if tc.wantParam != "" && f.linkQueries[0].Get("expires") != tc.wantParam {
				t.Errorf("--expires %s sent %q, want %q", tc.flag, f.linkQueries[0].Get("expires"), tc.wantParam)
			}
			if !strings.HasSuffix(stdout, "expires 2026-12-30T00:00:00Z\n") {
				t.Errorf("stdout = %q, want the token's expiry as an RFC 3339 instant", stdout)
			}
		})
	}
}

// Refused before anything is sent: no name, no scope, an --expires that is no
// shape the server takes.
func TestTokenCreateRefusesBadFlagsBeforeAnyRequest(t *testing.T) {
	cases := map[string][]string{
		"no name":      {"--scope", "data:read"},
		"no scope":     {"--name", "etl"},
		"bad expires":  {"--name", "etl", "--scope", "data:read", "--expires", "soon"},
		"blank scopes": {"--name", "etl", "--scope", " "},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTokenFake(t)
			opened := tokenBrowser(t, f)
			_, _, errText := runToken(t, args...)
			if errText == "" {
				t.Fatal("accepted")
			}
			if ev := f.eventsCopy(); len(ev) != 0 || len(*opened) != 0 {
				t.Errorf("sent %v / opened %v before refusing", ev, *opened)
			}
		})
	}
}

func TestTokenCreateCommaSeparatedScopes(t *testing.T) {
	f := newTokenFake(t, mineAnswer{tokens: []map[string]any{}})
	tokenBrowser(t, f)
	if _, _, errText := runToken(t, "--name", "etl", "--scope", "data:read,automation:write", "--no-wait"); errText != "" {
		t.Fatal(errText)
	}
	if got := strings.Join(f.linkQueries[0]["scope"], ","); got != "data:read,automation:write" {
		t.Errorf("scopes sent = %q", got)
	}
}

// token-link's refusals: each named, and no browser opens for any of them.
func TestTokenCreateLinkRefusals(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"not an admin", `{"error":"` + tokenAdminOnly + `"}`, http.StatusForbidden, tokenAdminOnly},
		{"scoped login", `{"error":"token scope \"admin\" does not permit read","code":"insufficient_scope"}`, http.StatusForbidden, tokenScopedLogin},
		{"older server", `{"error":"not found"}`, http.StatusNotFound, "this Ronja server is older than the CLI. Create the token in Ronja under Account → Access tokens."},
		{"bad scope", `{"error":"unknown scope \"dta\" (scopes: data, structure)"}`, http.StatusBadRequest, `unknown scope "dta"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTokenFake(t)
			f.linkStatus, f.linkBody = tc.status, tc.body
			opened := tokenBrowser(t, f)
			stdout, _, errText := runToken(t, "--name", "etl", "--scope", "data:read")
			if !strings.Contains(errText, tc.want) {
				t.Errorf("error = %q, want %q", errText, tc.want)
			}
			if tc.status == http.StatusForbidden && errText != tc.want {
				t.Errorf("error = %q, want exactly %q", errText, tc.want)
			}
			if len(*opened) != 0 {
				t.Errorf("a browser opened on a refused link")
			}
			if ev := f.eventsCopy(); len(ev) != 1 {
				t.Errorf("requests = %v, want only the token-link call", ev)
			}
			if stdout != "" {
				t.Errorf("stdout = %q", stdout)
			}
		})
	}
}

// A scoped login without admin 403s on token-mine: told so, and no browser.
func TestTokenCreateSnapshotForbiddenIsTheScopedLoginMessage(t *testing.T) {
	f := newTokenFake(t, mineAnswer{httpStatus: http.StatusForbidden})
	opened := tokenBrowser(t, f)
	_, _, errText := runToken(t, "--name", "etl", "--scope", "data:read")
	if errText != tokenScopedLogin {
		t.Errorf("error = %q, want %q", errText, tokenScopedLogin)
	}
	if len(*opened) != 0 {
		t.Errorf("a browser opened although the token list cannot be read")
	}
}

func TestTokenCreateTokenListGoneMidWaitIsTerminal(t *testing.T) {
	fastToken(t, 5*time.Second)
	f := newTokenFake(t,
		mineAnswer{tokens: []map[string]any{oldToken}},
		mineAnswer{httpStatus: http.StatusNotFound},
	)
	tokenBrowser(t, f)
	_, _, errText := runToken(t, "--name", "etl", "--scope", "data:read")
	if !strings.Contains(errText, "did not answer the token list (HTTP 404)") {
		t.Errorf("error = %q", errText)
	}
	if f.gets != 2 {
		t.Errorf("token-mine read %d times, want 2: a 404 is not retried", f.gets)
	}
}

func TestTokenCreateTimesOut(t *testing.T) {
	fastToken(t, 30*time.Millisecond)
	f := newTokenFake(t, mineAnswer{tokens: []map[string]any{oldToken}})
	tokenBrowser(t, f)
	stdout, _, errText := runToken(t, "--name", "etl", "--scope", "data:read")
	if !strings.HasPrefix(errText, `no new token named "etl" or with exactly the scopes asked for appeared within `) || !strings.HasSuffix(errText, "; if you created it, it is in Account → Access tokens") {
		t.Errorf("error = %q", errText)
	}
	if stdout != "" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestTokenCreateCtrlCStopsWaiting(t *testing.T) {
	fastToken(t, 10*time.Second)
	f := newTokenFake(t, mineAnswer{tokens: []map[string]any{oldToken}})
	tokenBrowser(t, f)
	var once sync.Once
	f.onGet = func(n int) {
		if n >= 2 { // the first poll: the wait loop has been entered
			once.Do(func() {
				if self, err := os.FindProcess(os.Getpid()); err == nil {
					_ = self.Signal(os.Interrupt)
				}
			})
		}
	}
	start := time.Now()
	stdout, _, errText := runToken(t, "--name", "etl", "--scope", "data:read")
	if !strings.HasPrefix(errText, "stopped waiting.") {
		t.Errorf("error = %q, want the stop", errText)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("Ctrl-C took %s to land", waited)
	}
	if stdout != "" {
		t.Errorf("stdout = %q", stdout)
	}
}

// --no-wait opens and returns without reading the token list at all;
// --no-browser never calls the browser and puts the link on stderr.
func TestTokenCreateNoWaitAndNoBrowser(t *testing.T) {
	f := newTokenFake(t)
	opened := tokenBrowser(t, f)
	stdout, stderr, errText := runToken(t, "--name", "etl", "--scope", "data:read", "--no-wait", "--no-browser")
	if errText != "" {
		t.Fatal(errText)
	}
	if len(*opened) != 0 {
		t.Errorf("--no-browser opened %v", *opened)
	}
	if !strings.Contains(stderr, "Open this link to create the token:\n  "+tokenLinkURL) {
		t.Errorf("stderr does not carry the link:\n%s", stderr)
	}
	if stdout != "" || f.gets != 0 {
		t.Errorf("stdout = %q, token-mine reads = %d; --no-wait never looks for the token, so it takes no snapshot", stdout, f.gets)
	}
}

// A sign-in token that lands DURING the wait (another terminal's `ronja login`,
// an MCP client) appears alone in an earlier poll: it is named on stderr and
// does not end the wait, and the form's token, a poll later, is the one
// reported. A sign-in token is full access, so it never matches on scopes.
func TestTokenCreateIgnoresAnUnrelatedTokenThatAppearsFirst(t *testing.T) {
	fastToken(t, 5*time.Second)
	login := tok("api_token-login", "ronja-cli on desktop", nil, nil)
	form := tok(tokenNewID, "etl", map[string]string{"data": "read"}, nil)
	f := newTokenFake(t,
		mineAnswer{tokens: []map[string]any{oldToken}},
		mineAnswer{tokens: []map[string]any{oldToken, login}},
		mineAnswer{tokens: []map[string]any{oldToken, login}},
		mineAnswer{tokens: []map[string]any{oldToken, login, form}},
	)
	tokenBrowser(t, f)
	stdout, stderr, errText := runToken(t, "--name", "etl", "--scope", "data:read")
	if errText != "" {
		t.Fatalf("token create failed: %s\nstderr: %s", errText, stderr)
	}
	if want := "created: " + tokenNewID + "  etl  data:read  expires never\n"; stdout != want {
		t.Errorf("stdout = %q, want only the form's token %q", stdout, want)
	}
	if f.gets != 4 {
		t.Errorf("token-mine read %d times, want 4: the sign-in token must not end the wait", f.gets)
	}
	notice := "A new token appeared that does not match: api_token-login  ronja-cli on desktop"
	if strings.Count(stderr, notice) != 1 {
		t.Errorf("stderr should name the unrelated token exactly once:\n%s", stderr)
	}
}

// Renamed on the form with the scopes left as asked: still this request's.
// Changed scopes with the name left: still this request's. Both changed: not.
func TestTokenCreateMatchesOnNameOrExactScopes(t *testing.T) {
	cases := []struct {
		name     string
		token    map[string]any
		reported bool
	}{
		{"renamed, same scopes", tok(tokenNewID, "Sales app", map[string]string{"data": "read", "automation": "write"}, nil), true},
		{"same name, other scopes", tok(tokenNewID, "etl", map[string]string{"data": "write"}, nil), true},
		{"same name, full access", tok(tokenNewID, "etl", nil, nil), true},
		{"renamed, fewer scopes", tok(tokenNewID, "Sales app", map[string]string{"data": "read"}, nil), false},
		{"renamed, more scopes", tok(tokenNewID, "Sales app", map[string]string{"data": "read", "automation": "write", "agents": "read"}, nil), false},
		{"renamed, full access", tok(tokenNewID, "Sales app", nil, nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastToken(t, 40*time.Millisecond)
			f := newTokenFake(t,
				mineAnswer{tokens: []map[string]any{oldToken}},
				mineAnswer{tokens: []map[string]any{oldToken, tc.token}},
			)
			tokenBrowser(t, f)
			stdout, stderr, errText := runToken(t, "--name", "etl", "--scope", "data:read", "--scope", "automation:write")
			if tc.reported {
				if errText != "" || !strings.HasPrefix(stdout, "created: "+tokenNewID+"  ") {
					t.Fatalf("not reported: stdout %q, error %q", stdout, errText)
				}
				return
			}
			if stdout != "" {
				t.Errorf("reported a token that matches neither the name nor the scopes: %q", stdout)
			}
			if !strings.Contains(errText, "New tokens that did not match: "+tokenNewID+" (Sales app)") {
				t.Errorf("the timeout does not list the unmatched token: %q", errText)
			}
			if !strings.Contains(stderr, "A new token appeared that does not match: "+tokenNewID) {
				t.Errorf("stderr does not name the unmatched token as it appeared:\n%s", stderr)
			}
		})
	}
}

// The expiry is an instant with its offset, not a bare date: the form sends
// midnight in the BROWSER's zone, and a bare date read in another zone is the
// day before. Sets time.Local, which is safe only because no test in this
// package calls t.Parallel.
func TestTokenCreatePrintsTheExpiryAsAnInstant(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("CET", 3600)
	t.Cleanup(func() { time.Local = saved })
	fastToken(t, 5*time.Second)
	// Midnight 2026-12-30 in Stockholm (UTC+1), as a browser there sends it.
	f := newTokenFake(t,
		mineAnswer{tokens: []map[string]any{}},
		mineAnswer{tokens: []map[string]any{tok(tokenNewID, "etl", map[string]string{"data": "read"}, "2026-12-29T23:00:00Z")}},
	)
	tokenBrowser(t, f)
	stdout, _, errText := runToken(t, "--name", "etl", "--scope", "data:read")
	if errText != "" {
		t.Fatal(errText)
	}
	if !strings.HasSuffix(stdout, "expires 2026-12-30T00:00:00+01:00\n") {
		t.Errorf("stdout = %q, want the instant in RFC 3339 with its offset", stdout)
	}
	time.Local = time.UTC
	if got := tokenExpiry(ptrTime(time.Date(2026, 12, 29, 23, 0, 0, 0, time.UTC))); got != "2026-12-29T23:00:00Z" {
		t.Errorf("in UTC: %q, want the same instant with its own offset", got)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// The organization the link was stamped with is named on stderr before the
// browser opens: by its id under an environment token (the server's ?org=),
// by the stored profile's name when the credential is that profile's.
func TestTokenCreateNamesTheOrganization(t *testing.T) {
	t.Run("environment token", func(t *testing.T) {
		f := newTokenFake(t)
		tokenBrowser(t, f)
		_, stderr, errText := runToken(t, "--name", "etl", "--scope", "data:read", "--no-wait")
		if errText != "" {
			t.Fatal(errText)
		}
		if !strings.Contains(stderr, "Creating a token for organization ten-test.\n") {
			t.Errorf("stderr does not name the organization by the link's id:\n%s", stderr)
		}
	})
	t.Run("stored profile", func(t *testing.T) {
		f := newTokenFake(t)
		t.Setenv("RONJA_TOKEN", "")
		writeProfiles(t, "acme", testProfile{Name: "acme", URL: f.server.URL, TenantID: "ten-test", TenantName: "Acme AB", Token: "test-token"})
		tokenBrowser(t, f)
		_, stderr, errText := runToken(t, "--name", "etl", "--scope", "data:read", "--no-wait")
		if errText != "" {
			t.Fatal(errText)
		}
		if !strings.Contains(stderr, "Creating a token for organization Acme AB.\n") {
			t.Errorf("stderr does not name the organization by the profile's name:\n%s", stderr)
		}
	})
}

// The command's own flag set offers no way to pre-select full access.
func TestTokenCreateHasNoFullAccessFlag(t *testing.T) {
	cmd := newTokenCreateCmd()
	for _, name := range []string{"full-access", "full", "access", "json", "print", "out"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("token create has a --%s flag", name)
		}
	}
}

// Parse actual stdout, including the no-wait path: JSON is one complete object,
// and multiple matches retain their metadata without ever decoding token values.
func TestTokenCreateJSON(t *testing.T) {
	for _, noWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "pending"}[noWait], func(t *testing.T) {
			fastToken(t, 5*time.Second)
			f := newTokenFake(t,
				mineAnswer{tokens: []map[string]any{oldToken}},
				mineAnswer{tokens: []map[string]any{oldToken,
					tok(tokenNewID, "etl", map[string]string{"data": "read"}, "2026-12-30T00:00:00Z"),
					tok("api_token-renamed", "Renamed", map[string]string{"data": "read"}, nil),
				}},
			)
			tokenBrowser(t, f)
			args := []string{"--json", "--name", "etl", "--scope", "data:read", "--no-browser"}
			if noWait {
				args = append(args, "--no-wait")
			}
			stdout, stderr, errText := runToken(t, args...)
			if errText != "" {
				t.Fatal(errText)
			}
			var got struct {
				Status string `json:"status"`
				Tokens []struct {
					ID          string            `json:"id"`
					Name        string            `json:"name"`
					ScopeGrants map[string]string `json:"scopeGrants"`
					ExpiresAt   *time.Time        `json:"expiresAt"`
				} `json:"tokens"`
			}
			decoder := json.NewDecoder(strings.NewReader(stdout))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&got); err != nil {
				t.Fatalf("invalid JSON result %q: %v", stdout, err)
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				t.Fatalf("extra stdout after JSON: %q", stdout)
			}
			if strings.Contains(stdout, tokenLinkURL) || !strings.Contains(stderr, tokenLinkURL) {
				t.Fatal("link must stay on stderr")
			}
			if noWait {
				if got.Status != "pending" || got.Tokens == nil || len(got.Tokens) != 0 {
					t.Fatalf("unexpected no-wait result: %s", stdout)
				}
				if f.gets != 0 {
					t.Fatal("no-wait read the token list")
				}
				return
			}
			if got.Status != "created" || len(got.Tokens) != 2 {
				t.Fatalf("unexpected created result: %s", stdout)
			}
			first, second := got.Tokens[0], got.Tokens[1]
			if first.ID != tokenNewID || first.Name != "etl" || first.ScopeGrants["data"] != "read" || first.ExpiresAt == nil || first.ExpiresAt.Format(time.RFC3339) != "2026-12-30T00:00:00Z" {
				t.Fatalf("wrong first token metadata: %s", stdout)
			}
			if second.ID != "api_token-renamed" || second.Name != "Renamed" || second.ExpiresAt != nil {
				t.Fatalf("wrong second token metadata: %s", stdout)
			}
		})
	}
}
