package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/config"
)

// `ronja api` has no endpoint knowledge to test, which is the point of it. What
// is left is the layer that CAN be wrong: how a request is assembled from
// flags, and what the exit code says afterwards. Both are contracts an agent
// depends on and neither is visible from the output.

// echoServer records every request and answers with whatever the test set.
type echoServer struct {
	t        *testing.T
	server   *httptest.Server
	requests []echoedRequest

	// status and body are the answer. Zero status means 200.
	status int
	body   string

	// script overrides status/body per request, in order, with the LAST entry
	// repeating forever. That is what makes both "it becomes ready on the third
	// poll" and "it never does" expressible without a second knob — the same
	// mechanism fakeInstance uses for workflow runs.
	script []echoedReply
	// replyHeader is added to every response, for the one header a client is
	// required to act on (Retry-After).
	replyHeader http.Header
}

// echoedReply is one scripted answer.
type echoedReply struct {
	Status int
	Body   string
}

type echoedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
	// Host is the request line's authority, which net/http keeps OFF the header
	// map on both sides — so it has to be recorded separately or a test cannot
	// see it at all.
	Host string
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	e := &echoServer{t: t}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.requests = append(e.requests, echoedRequest{
			Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(),
			Body: string(body), Host: r.Host,
		})
		status, replyBody := e.status, e.body
		if len(e.script) > 0 {
			// Served requests are counted before this point, so the reply for
			// request N is entry N-1, and the last entry repeats.
			i := len(e.requests) - 1
			if i >= len(e.script) {
				i = len(e.script) - 1
			}
			status, replyBody = e.script[i].Status, e.script[i].Body
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		for name, values := range e.replyHeader {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(status)
		if replyBody == "" {
			replyBody = `{"ok":true}`
		}
		_, _ = io.WriteString(w, replyBody)
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *echoServer) URL() string { return e.server.URL }

// only returns the single request served, failing when there was not exactly
// one — a command that made two round trips where one was expected is a bug
// worth failing on rather than indexing past.
func (e *echoServer) only() echoedRequest {
	e.t.Helper()
	if len(e.requests) != 1 {
		e.t.Fatalf("served %d requests, want 1: %+v", len(e.requests), e.requests)
	}
	return e.requests[0]
}

// --- method defaulting ------------------------------------------------------

// No body, no -X: a read. This is the form an agent types most, so the default
// has to be the harmless one.
func TestAPIDefaultsToGET(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "/api/v2/authentication/me")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	req := e.only()
	if req.Method != "GET" {
		t.Errorf("method = %s, want GET", req.Method)
	}
	if req.Path != "/api/v2/authentication/me" {
		t.Errorf("path = %s", req.Path)
	}
	// The body is the output, verbatim and alone.
	if out != `{"ok":true}` {
		t.Errorf("stdout = %q, want the response body verbatim", out)
	}
}

// A body with no -X means POST, curl-style. Getting this wrong sends a body on
// a GET, which most servers quietly ignore — so the failure is a silent no-op
// rather than an error, which is exactly why it is pinned.
func TestAPIDefaultsToPOSTWhenDataIsGiven(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "-d", `{"name":"Sales"}`, "/api/v2/feature"); err != nil {
		t.Fatalf("api: %v", err)
	}
	req := e.only()
	if req.Method != "POST" {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.Body != `{"name":"Sales"}` {
		t.Errorf("body = %q", req.Body)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json by default", got)
	}
}

// An explicit -X still wins over the body's default.
func TestAPIExplicitMethodWinsOverTheBodyDefault(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "-X", "put", "-d", "{}", "/api/v2/feature/f1"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if req := e.only(); req.Method != "PUT" {
		t.Errorf("method = %s, want PUT (upper-cased)", req.Method)
	}
}

// --- the body ---------------------------------------------------------------

// @file, because a JSON body big enough to matter does not belong on a command
// line where the shell gets a vote on its contents.
func TestAPIReadsTheBodyFromAFile(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	dir := t.TempDir()
	path := filepath.Join(dir, "feature.json")
	const payload = "{\n  \"name\": \"Sales\"\n}\n"
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, dir, "api", "-d", "@"+path, "/api/v2/feature"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if req := e.only(); req.Body != payload {
		t.Errorf("body = %q, want the file verbatim", req.Body)
	}
}

// @- is the pipe, which is how an agent that generated a body hands it over
// without writing it to disk first.
func TestAPIReadsTheBodyFromStdin(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	withStdin(t, `{"piped":true}`)

	if _, err := runCLI(t, t.TempDir(), "api", "-d", "@-", "/api/v2/feature"); err != nil {
		t.Fatalf("api: %v", err)
	}
	req := e.only()
	if req.Body != `{"piped":true}` {
		t.Errorf("body = %q", req.Body)
	}
	if req.Method != "POST" {
		t.Errorf("method = %s, want POST — a piped body is still a body", req.Method)
	}
}

// The rule the whole file exists for: a read of a terminal does not fail, it
// waits forever. An agent cannot tell that from a crash.
func TestAPIRefusesToReadStdinFromATerminal(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	withTerminalStdin(t)

	_, err := runCLI(t, t.TempDir(), "api", "-d", "@-", "/api/v2/feature")
	if err == nil {
		t.Fatal("reading a terminal was accepted; it would have hung")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// An oversized @file is refused before the request goes out, naming the limit.
// The body is buffered whole, so an unbounded read is an unbounded allocation
// chosen by whatever path was typed.
func TestAPIRefusesAnOversizedBodyFile(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.json")
	if err := os.WriteFile(path, make([]byte, maxRequestBody+1), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, dir, "api", "-d", "@"+path, "/api/v2/feature")
	if err == nil {
		t.Fatal("an oversized body file was accepted")
	}
	if !strings.Contains(err.Error(), "MiB") {
		t.Errorf("error does not name the limit: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// ...and a body exactly at the limit is not: an off-by-one here would refuse a
// legitimate payload with a message about a limit it did not exceed.
func TestAPIAcceptsABodyAtTheLimit(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	payload := make([]byte, maxRequestBody)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, dir, "api", "-d", "@"+path, "/api/v2/feature"); err != nil {
		t.Fatalf("a body exactly at the limit was refused: %v", err)
	}
	if got := len(e.only().Body); got != maxRequestBody {
		t.Errorf("body = %d bytes, want %d", got, maxRequestBody)
	}
}

// --- the deadline -----------------------------------------------------------

// Zero is "wait as long as it takes", so it must not be read as a deadline that
// has already passed — the shape context.WithTimeout(ctx, 0) would give it.
// Mirrors `ronja query`.
func TestAPITimeoutZeroWaits(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--timeout", "0", "/api/v2/thing")
	if err != nil {
		t.Fatalf("--timeout 0 failed the request: %v", err)
	}
	if out != `{"ok":true}` {
		t.Errorf("stdout = %q", out)
	}
}

func TestAPIRefusesANegativeTimeout(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "--timeout", "-5s", "/api/v2/thing")
	if err == nil {
		t.Fatal("a negative timeout was accepted")
	}
	if !strings.Contains(err.Error(), "pass 0") {
		t.Errorf("error does not say what 0 means: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// --- headers ----------------------------------------------------------------

func TestAPIHeaderOverridesTheDefaultContentType(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api",
		"-H", "Content-Type: text/csv", "-H", "X-Trace: abc",
		"-d", "a,b\n", "/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
	req := e.only()
	if got := req.Header.Get("Content-Type"); got != "text/csv" {
		t.Errorf("Content-Type = %q, want the supplied one to win", got)
	}
	if got := req.Header.Get("X-Trace"); got != "abc" {
		t.Errorf("X-Trace = %q", got)
	}
}

func TestAPIRejectsAMalformedHeader(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "-H", "no-colon-here", "/api/v2/thing")
	if err == nil {
		t.Fatal("a header with no colon was accepted")
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// The credential is attached without anything having to echo it — the entire
// reason this command is preferable to the eval-and-curl form.
func TestAPISendsTheBearerToken(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if got := e.only().Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q", got)
	}
}

// --- failure ----------------------------------------------------------------

// Three things at once on an HTTP error, and all three are contracts: the body
// still reaches stdout (it is what the server said about the failure), ONE line
// reaches stderr, and the exit code is non-zero.
func TestAPIReportsAnHTTPErrorWithoutSwallowingTheBody(t *testing.T) {
	e := newEchoServer(t)
	e.status = http.StatusUnprocessableEntity
	e.body = `{"error":"name is required"}`
	signInTo(t, e.URL())

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, t.TempDir(), "api", "-X", "POST", "-d", "{}", "/api/v2/feature")
	})
	if err == nil {
		t.Fatal("exit code was zero on a 422")
	}
	if out != `{"error":"name is required"}` {
		t.Errorf("stdout = %q, want the error body", out)
	}
	if want := "HTTP 422 POST /api/v2/feature\n"; stderr != want {
		t.Errorf("stderr = %q, want exactly %q", stderr, want)
	}
}

// The same failure through the REAL entry point, which is where the sentinel
// suppression lives. runCLI drives root.Execute() directly and so never reaches
// it — meaning the "exactly one line" contract could be broken without any
// other test in this file noticing.
func TestExecuteReportsAnHTTPErrorExactlyOnce(t *testing.T) {
	e := newEchoServer(t)
	e.status = http.StatusNotFound
	e.body = `{"error":"not found"}`
	signInTo(t, e.URL())
	t.Chdir(t.TempDir())

	var code int
	var out string
	stderr := captureStderr(t, func() {
		stdout, restore := captureStdout(t)
		code = run([]string{"api", "/api/v2/thing"})
		out = restore(stdout)
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if out != `{"error":"not found"}` {
		t.Errorf("stdout = %q, want the error body", out)
	}
	// Exactly this, and nothing else: no "error: already reported" underneath,
	// and no cobra usage dump either.
	if want := "HTTP 404 GET /api/v2/thing\n"; stderr != want {
		t.Errorf("stderr = %q, want exactly %q", stderr, want)
	}
}

// ...and the suppression must not be over-broad: an ordinary failure, which has
// said nothing for itself, still gets its one reported line.
func TestExecuteStillReportsAnOrdinaryFailure(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	t.Chdir(t.TempDir())

	var code int
	stderr := captureStderr(t, func() {
		code = run([]string{"api", "not-rooted"})
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(stderr, "error: ") {
		t.Errorf("stderr = %q, want the reported form", stderr)
	}
	if strings.Count(stderr, "\n") != 1 {
		t.Errorf("stderr is not one line: %q", stderr)
	}
}

// Host is the one header net/http takes from req.Host rather than the header
// map, so a bare Set leaves it silently dropped — the failure being that the
// flag does nothing at all, with no way to tell from the outside.
func TestAPIHonoursAHostHeader(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "-H", "Host: tenant.example", "/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if got := e.only().Host; got != "tenant.example" {
		t.Errorf("Host = %q, want it to reach the request line", got)
	}
}

// A transport failure is the other exit-non-zero path, and it must not name the
// credential in the process.
func TestAPIReportsATransportFailure(t *testing.T) {
	// A port nothing is listening on, reached the same way a real run would.
	signInTo(t, "http://127.0.0.1:1")

	_, err := runCLI(t, t.TempDir(), "api", "/api/v2/thing")
	if err == nil {
		t.Fatal("a dead instance produced no error")
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("the token leaked into an error message: %v", err)
	}
}

func TestAPIRefusesAPathThatIsNotRooted(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "api/v2/thing")
	if err == nil {
		t.Fatal("a relative path was accepted")
	}
	if !strings.Contains(err.Error(), `must start with "/"`) {
		t.Errorf("error does not say what to fix: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// --- credential resolution --------------------------------------------------

// RONJA_TOKEN outranking the file is what keeps a CI job from acting as
// whoever last logged in on the machine. Asserted through the command rather
// than through config.Resolve, because the command is where a future refactor
// would break it.
func TestAPIPrefersTheEnvironmentOverTheCredentialFile(t *testing.T) {
	e := newEchoServer(t)
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)
	writeCredentialFile(t, dir, e.URL(), "file-token")
	t.Setenv("RONJA_URL", e.URL())
	t.Setenv("RONJA_TOKEN", "env-token")

	if _, err := runCLI(t, t.TempDir(), "api", "/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if got := e.only().Header.Get("Authorization"); got != "Bearer env-token" {
		t.Errorf("Authorization = %q, want the environment's token to win", got)
	}
}

// ...and the file is still used when the environment is silent, which is the
// other half of the same contract.
func TestAPIFallsBackToTheCredentialFile(t *testing.T) {
	e := newEchoServer(t)
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)
	writeCredentialFile(t, dir, e.URL(), "file-token")
	t.Setenv("RONJA_URL", e.URL())
	t.Setenv("RONJA_TOKEN", "")

	if _, err := runCLI(t, t.TempDir(), "api", "/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if got := e.only().Header.Get("Authorization"); got != "Bearer file-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestAPIRefusesWhenSignedOut(t *testing.T) {
	e := newEchoServer(t)
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	t.Setenv("RONJA_URL", e.URL())
	t.Setenv("RONJA_TOKEN", "")

	_, err := runCLI(t, t.TempDir(), "api", "/api/v2/thing")
	if err == nil {
		t.Fatal("an unauthenticated call was attempted")
	}
	if !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("error does not say what to do: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// --- shared helpers ---------------------------------------------------------

// withStdin points os.Stdin at a temp file holding content — never a terminal,
// which is what makes it the "piped in" case.
func withStdin(t *testing.T, content string) {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("create stdin file: %v", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		t.Fatalf("write stdin file: %v", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind stdin file: %v", err)
	}
	saved := os.Stdin
	os.Stdin = tmp
	t.Cleanup(func() {
		os.Stdin = saved
		tmp.Close()
	})
}

// withTerminalStdin makes isTerminal say yes, which is the state no test can
// otherwise reach without allocating a pty.
func withTerminalStdin(t *testing.T) {
	t.Helper()
	saved := isTerminal
	isTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminal = saved })
}

// writeCredentialFile seeds a config.json the way a real login would leave one.
func writeCredentialFile(t *testing.T, dir, url, token string) {
	t.Helper()
	body := `{"currentProfile":"test","profiles":{"test":{"url":` + quoteJSON(url) +
		`,"tenantID":"ten-test","token":` + quoteJSON(token) + `}}}`
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", config.FileName, err)
	}
}

func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
