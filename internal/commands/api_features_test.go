package commands

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for `ronja api`'s --jq, -F, --out, --retry, --fail-on-error and
// --wait-until, pinning the rules invisible from the output.

// --- --jq -------------------------------------------------------------------

func TestAPIJQFiltersTheResponse(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"result":[{"id":"wf-1"},{"id":"wf-2"}]}`
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--jq", ".result[].id", "/api/v2/workflow/query")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if out != "\"wf-1\"\n\"wf-2\"\n" {
		t.Errorf("stdout = %q", out)
	}
}

// A quoted "wf-1" substituted into a path via $(...) is a path that does not
// exist.
func TestAPIJQRawUnquotesStrings(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"id":"wf-1"}`
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--jq", ".id", "-r", "/api/v2/workflow/wf-1")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if out != "wf-1\n" {
		t.Errorf("stdout = %q, want unquoted", out)
	}
}

// A large listing is printed in full, with or without --fail-on-error: a cap
// would capture a silently wrong prefix and exit zero. 20,000 is twice a 10,000
// cap rather than at its boundary, so an off-by-one reintroducing one still fails.
func TestAPIJQPrintsEveryValueOfALargeListing(t *testing.T) {
	const items = 20000

	e := newEchoServer(t)
	var body strings.Builder
	body.WriteString(`{"result":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":"wf-%d"}`, i)
	}
	body.WriteString(`]}`)
	e.body = body.String()
	signInTo(t, e.URL())

	for _, args := range [][]string{
		{"api", "--jq", ".result[].id", "-r", "/api/v2/feature/query"},
		{"api", "--fail-on-error", "--jq", ".result[].id", "-r", "/api/v2/feature/query"},
	} {
		t.Run(strings.Join(args[1:len(args)-1], " "), func(t *testing.T) {
			out, err := runCLI(t, t.TempDir(), args...)
			if err != nil {
				t.Fatalf("a large listing must not be a failure: %v", err)
			}
			if got := strings.Count(out, "\n"); got != items {
				t.Errorf("printed %d values, want all %d", got, items)
			}
			if !strings.HasPrefix(out, "wf-0\nwf-1\n") ||
				!strings.HasSuffix(out, fmt.Sprintf("wf-%d\n", items-1)) {
				t.Errorf("stdout is not the whole stream in order: starts %.12q, ends %.12q",
					out, out[len(out)-12:])
			}
		})
	}
}

// --jq-strict must stay an unknown flag: with no truncation there is nothing for
// it to make strict, and a no-op would tell a script it is guarded when it is not.
func TestAPIHasNoJQStrictFlag(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"result":[]}`
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "--jq-strict", "--jq", ".result[].id", "/api/v2/feature/query")
	if err == nil {
		t.Fatal("--jq-strict was accepted; it must not exist any more")
	}
	if !strings.Contains(err.Error(), "jq-strict") {
		t.Errorf("the refusal should name the unknown flag, got: %v", err)
	}
}

// A jq stream can fail late, so a failing filter must print nothing, not a
// prefix a caller redirecting stdout would keep.
func TestAPIJQPrintsNothingWhenTheFilterFailsLate(t *testing.T) {
	e := newEchoServer(t)
	var body strings.Builder
	body.WriteString(`{"result":[`)
	for i := 0; i < 200; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, "%d", i)
	}
	body.WriteString(`]}`)
	e.body = body.String()
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api",
		"--jq", `.result[] | if . == 150 then . + "boom" else . end`, "/api/v2/feature/query")
	if err == nil {
		t.Fatal("a filter that fails partway must not exit zero")
	}
	if out != "" {
		t.Errorf("stdout = %.60q, want nothing at all", out)
	}
}

// A bad expression must be refused before any request (or --wait-until poll).
func TestAPIRefusesABadJQExpressionBeforeSending(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "--jq", ".result[", "/api/v2/workflow/query")
	if err == nil {
		t.Fatal("a malformed jq expression was accepted")
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out for an expression that cannot compile: %+v", e.requests)
	}
}

func TestAPIRawWithoutJQIsRefused(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "-r", "/api/v2/thing")
	if err == nil {
		t.Fatal("--raw was accepted without --jq")
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// --- -F multipart -----------------------------------------------------------

func TestAPIFormSendsMultipart(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	dir := t.TempDir()
	path := filepath.Join(dir, "quarterly.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.7 fake"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := runCLI(t, dir, "api", "-X", "POST", "/api/v2/file/upload/uploads",
		"-F", "file=@"+path, "-F", "note=draft"); err != nil {
		t.Fatalf("api: %v", err)
	}
	req := e.only()
	if ct := req.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type = %q, want multipart with a boundary", ct)
	}
	// A part without a filename is a plain field most servers ignore.
	if !strings.Contains(req.Body, `filename="quarterly.pdf"`) {
		t.Errorf("body does not carry the filename:\n%s", req.Body)
	}
	// Ronja records the type on the file row; octet-stream costs preview and
	// text extraction.
	if !strings.Contains(req.Body, "Content-Type: application/pdf") {
		t.Errorf("body does not carry a guessed content type:\n%s", req.Body)
	}
	if !strings.Contains(req.Body, "draft") {
		t.Errorf("body is missing the literal field:\n%s", req.Body)
	}
}

func TestAPIFormRefusesAMissingFileBeforeSending(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "-X", "POST", "/api/v2/file/upload/x",
		"-F", "file=@/nonexistent/nope.pdf")
	if err == nil {
		t.Fatal("a missing -F file was accepted")
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// A second reader of a drained pipe sends an EMPTY part the server accepts.
func TestAPIRefusesTwoFormFieldsReadingStdin(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "-X", "POST", "/api/v2/file/upload/x",
		"-F", "one=@-", "-F", "two=@-")
	if err == nil {
		t.Fatal("two -F fields both reading stdin were accepted")
	}
	if !strings.Contains(err.Error(), "one=@-") || !strings.Contains(err.Error(), "two=@-") {
		t.Errorf("the error does not name both readers: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// A caller-supplied boundary would parse as ZERO parts and report success, so
// the subtype is honoured and the boundary replaced.
func TestAPIFormRewritesASuppliedMultipartBoundary(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := runCLI(t, dir, "api", "-X", "POST", "/api/v2/file/upload/x",
		"-F", "file=@"+path,
		"-H", "Content-Type: multipart/related; boundary=WRONG"); err != nil {
		t.Fatalf("api: %v", err)
	}

	req := e.only()
	ct := req.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/related;") {
		t.Errorf("the supplied subtype was discarded: %q", ct)
	}
	if strings.Contains(ct, "WRONG") {
		t.Fatalf("the stale boundary survived: %q", ct)
	}
	// Asserted against the body, not just against the string "WRONG".
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", ct, err)
	}
	if b := params["boundary"]; b == "" || !strings.Contains(req.Body, b) {
		t.Errorf("declared boundary %q does not appear in the body", b)
	}
}

func TestAPIFormRefusesANonMultipartContentType(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := runCLI(t, dir, "api", "-X", "POST", "/api/v2/file/upload/x",
		"-F", "file=@"+path, "-H", "Content-Type: application/json")
	if err == nil {
		t.Fatal("a non-multipart Content-Type was accepted alongside -F")
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

func TestAPIRefusesDataAndFormTogether(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "/api/v2/thing", "-d", "{}", "-F", "a=b")
	if err == nil {
		t.Fatal("-d and -F were accepted together")
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

func TestAPIFormEscapesAFilename(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	dir := t.TempDir()
	path := filepath.Join(dir, `we"ird.txt`)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Skipf("this filesystem will not hold a quote in a filename: %v", err)
	}
	if _, err := runCLI(t, dir, "api", "-X", "POST", "/api/v2/file/upload/x", "-F", "file=@"+path); err != nil {
		t.Fatalf("api: %v", err)
	}
	if !strings.Contains(e.only().Body, `filename="we\"ird.txt"`) {
		t.Errorf("filename was not escaped:\n%s", e.only().Body)
	}
}

// --- --out ------------------------------------------------------------------

func TestAPIOutWritesTheBodyOwnerOnly(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"big":"payload"}`
	signInTo(t, e.URL())

	dir := t.TempDir()
	target := filepath.Join(dir, "resp.json")
	out, err := runCLI(t, dir, "api", "-o", target, "/api/v2/thing")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	// stdout stays empty, as with `query --out`: a duplicate would land in
	// whatever a pipeline is parsing.
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if got := readFile(t, target); got != e.body {
		t.Errorf("file = %q, want %q", got, e.body)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o, want 0600 — this is tenant data", perm)
	}
}

// os.WriteFile would inherit a pre-existing file's world-readable mode; the
// temp-file-and-rename does not.
func TestAPIOutDoesNotInheritALooseMode(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	dir := t.TempDir()
	target := filepath.Join(dir, "resp.json")
	if err := os.WriteFile(target, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := runCLI(t, dir, "api", "-o", target, "/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o, want 0600", perm)
	}
}

func TestAPIOutAndJQTogether(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"id":"wf-9","extra":"kept"}`
	signInTo(t, e.URL())

	dir := t.TempDir()
	target := filepath.Join(dir, "resp.json")
	out, err := runCLI(t, dir, "api", "-o", target, "--jq", ".id", "-r", "/api/v2/thing")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if out != "wf-9\n" {
		t.Errorf("stdout = %q", out)
	}
	if got := readFile(t, target); got != e.body {
		t.Errorf("file = %q, want the verbatim body", got)
	}
}

// --- --fail-on-error --------------------------------------------------------

// A failed query is HTTP 200 with `error` set.
func TestAPIFailOnErrorCatchesA200Envelope(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"result":"","error":"Binder Error: no such column"}`
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--fail-on-error", "-X", "POST", "/api/v2/duckdb/query", "-d", `{"sql":"x"}`)
	if err == nil {
		t.Fatal("a 200 carrying an error exited zero")
	}
	if !strings.Contains(err.Error(), "Binder Error") {
		t.Errorf("the server's message is not in the error: %v", err)
	}
	if !strings.Contains(out, "Binder Error") {
		t.Errorf("the body was swallowed: %q", out)
	}
}

// Opt-in, because reading inside the body means holding all of it.
func TestAPIWithoutFailOnErrorA200EnvelopeStillExitsZero(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"error":"nope"}`
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
}

func TestAPIFailOnErrorIgnoresANonJSONBody(t *testing.T) {
	e := newEchoServer(t)
	e.body = "region,total\nEU,5\n"
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--fail-on-error", "/api/v2/thing")
	if err != nil {
		t.Fatalf("a CSV body was treated as an error: %v", err)
	}
	if out != e.body {
		t.Errorf("stdout = %q", out)
	}
}

// --- --retry ----------------------------------------------------------------

func TestAPIRetriesA503AndSucceeds(t *testing.T) {
	e := newEchoServer(t)
	e.script = []echoedReply{
		{Status: http.StatusServiceUnavailable, Body: `{"error":"restarting"}`},
		{Status: http.StatusOK, Body: `{"ok":true}`},
	}
	// A server-named delay, so the retry does not sleep a real second.
	e.replyHeader = http.Header{"Retry-After": []string{"0"}}
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--retry", "3", "/api/v2/thing")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if out != `{"ok":true}` {
		t.Errorf("stdout = %q", out)
	}
	if len(e.requests) != 2 {
		t.Errorf("made %d requests, want 2", len(e.requests))
	}
}

func TestAPIDoesNotRetryA400(t *testing.T) {
	e := newEchoServer(t)
	e.status = http.StatusBadRequest
	e.body = `{"error":"missing field"}`
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "--retry", "3", "/api/v2/thing"); err == nil {
		t.Fatal("a 400 exited zero")
	}
	if len(e.requests) != 1 {
		t.Errorf("made %d requests, want 1 — a 4xx must not be retried", len(e.requests))
	}
}

func TestAPIRetryGivesUpAndReportsTheLastAnswer(t *testing.T) {
	e := newEchoServer(t)
	e.status = http.StatusBadGateway
	e.body = `{"error":"upstream"}`
	e.replyHeader = http.Header{"Retry-After": []string{"0"}}
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--retry", "2", "/api/v2/thing")
	if err == nil {
		t.Fatal("an exhausted retry budget exited zero")
	}
	// 1 original + 2 retries.
	if len(e.requests) != 3 {
		t.Errorf("made %d requests, want 3", len(e.requests))
	}
	if !strings.Contains(out, "upstream") {
		t.Errorf("the final body was swallowed: %q", out)
	}
}

// --- --wait-until -----------------------------------------------------------

func TestAPIWaitUntilPollsToATerminalState(t *testing.T) {
	e := newEchoServer(t)
	e.script = []echoedReply{
		{Status: http.StatusOK, Body: `{"status":"running"}`},
		{Status: http.StatusOK, Body: `{"status":"running"}`},
		{Status: http.StatusOK, Body: `{"status":"success","rows":12}`},
		// A DIFFERENT terminal answer makes a re-fetch after the condition held
		// visible in stdout.
		{Status: http.StatusOK, Body: `{"status":"reaped"}`},
	}
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api",
		"--wait-until", `.status != "running"`, "--wait-interval", "1ms",
		"--jq", ".status", "-r", "/api/v2/workflow/run/run-1")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	// The emitted answer must be the response the condition MATCHED, not a
	// re-fetch.
	if out != "success\n" {
		t.Errorf("stdout = %q, want the matching response", out)
	}
	if len(e.requests) != 3 {
		t.Errorf("made %d requests, want 3", len(e.requests))
	}
}

// Re-sending a POST once per poll would create one resource per attempt.
func TestAPIWaitUntilRefusesANonIdempotentMethod(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "-X", "POST", "/api/v2/workflow/wf-1/run",
		"--wait-until", ".status")
	if err == nil {
		t.Fatal("--wait-until was accepted on a POST")
	}
	if !strings.Contains(err.Error(), "GET") {
		t.Errorf("the error does not say what is accepted: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

func TestAPIWaitUntilTimesOutNamingTheCondition(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"status":"running"}`
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api",
		"--wait-until", `.status == "done"`, "--wait-interval", "1ms", "--wait-timeout", "30ms",
		"/api/v2/workflow/run/run-1")
	if err == nil {
		t.Fatal("a wait that never came true exited zero")
	}
	// The timeout must name the condition, not "context deadline exceeded".
	if !strings.Contains(err.Error(), `.status == "done"`) {
		t.Errorf("the timeout does not name the condition: %v", err)
	}
}

// A 404 on the polled path is a typo, not "not ready yet".
func TestAPIWaitUntilStopsOnAnHTTPError(t *testing.T) {
	e := newEchoServer(t)
	e.status = http.StatusNotFound
	e.body = `{"error":"not found"}`
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api",
		"--wait-until", ".status", "--wait-interval", "1ms", "--wait-timeout", "5s",
		"/api/v2/workflow/run/nope")
	if err == nil {
		t.Fatal("a 404 during a wait exited zero")
	}
	if len(e.requests) != 1 {
		t.Errorf("made %d requests, want 1 — a 404 must end the wait", len(e.requests))
	}
}

// In jq an empty array is TRUE.
func TestAPIWaitUntilAnEmptyArrayIsTruthy(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"errors":[]}`
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api",
		"--wait-until", ".errors", "--wait-interval", "1ms", "--wait-timeout", "5s",
		"/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if len(e.requests) != 1 {
		t.Errorf("made %d requests, want 1 — [] satisfies a bare condition in jq", len(e.requests))
	}
}

// A condition with NO OUTPUT (a `select(...)` matching nothing) must not end
// the wait by vacuous truth.
func TestAPIWaitUntilNoOutputDoesNotSatisfy(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"runs":[{"state":"running"}]}`
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api",
		"--wait-until", `.runs[] | select(.state == "done")`, "--wait-interval", "1ms", "--wait-timeout", "30ms",
		"/api/v2/thing")
	if err == nil {
		t.Fatal("a condition that matched nothing satisfied the wait")
	}
	if len(e.requests) < 2 {
		t.Errorf("made %d requests, want it to keep polling", len(e.requests))
	}
}

// A comparison over an ABSENT field yields one output, `false`, not zero.
func TestAPIWaitUntilAFalseConditionDoesNotSatisfy(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"other":1}`
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api",
		"--wait-until", `.status == "done"`, "--wait-interval", "1ms", "--wait-timeout", "30ms",
		"/api/v2/thing")
	if err == nil {
		t.Fatal("a false condition satisfied the wait")
	}
	if len(e.requests) < 2 {
		t.Errorf("made %d requests, want it to keep polling", len(e.requests))
	}
}

func TestAPIWaitIntervalMustBePositive(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "--wait-until", ".x", "--wait-interval", "0", "/api/v2/thing")
	if err == nil {
		t.Fatal("a zero wait interval was accepted")
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// --- sleepCtx ---------------------------------------------------------------

// With a zero duration both select cases are ready and select picks at RANDOM;
// a cancelled context must win every time.
func TestSleepCtxAlwaysHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 200; i++ {
		if err := sleepCtx(ctx, 0); err == nil {
			t.Fatalf("sleepCtx returned nil for a cancelled context on iteration %d", i)
		}
	}
}

func TestSleepCtxWaitsWhenLive(t *testing.T) {
	start := time.Now()
	if err := sleepCtx(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("sleepCtx: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Errorf("returned after %s, want it to have actually waited", elapsed)
	}
}

// --- the --jq flag-order footgun ---------------------------------------------

// `--jq -r '.id'` binds "-r" as the EXPRESSION, and cobra's "accepts 1 arg(s)"
// would hide that; the refusal must name the flag-order fix.
func TestAPIRefusesAFlagAsTheJQExpression(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "--jq", "-r", ".id", "/api/v2/thing")
	if err == nil {
		t.Fatal("a flag was accepted as the --jq expression")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--jq") || !strings.Contains(msg, `"-r"`) {
		t.Errorf("the refusal names neither the flag nor what it got: %v", err)
	}
	if !strings.Contains(msg, "--jq '.id' -r") {
		t.Errorf("the refusal does not show the working form: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// --json is inherited from the root, so the check must read the complete flag
// set, not the local one.
func TestAPIRefusesAnInheritedFlagAsTheJQExpression(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "--jq", "--json", "/api/v2/thing")
	if err == nil {
		t.Fatal("a persistent root flag was accepted as the --jq expression")
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// A leading dash is ordinary jq: "-1" is negative one.
func TestAPIAllowsADashLeadingJQExpression(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"id":"wf-1"}`
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--jq", "-1", "/api/v2/thing")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if out != "-1\n" {
		t.Errorf("stdout = %q", out)
	}
	// The form the refusal recommends really works.
	out, err = runCLI(t, t.TempDir(), "api", "--jq", ".id", "-r", "/api/v2/thing")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if out != "wf-1\n" {
		t.Errorf("stdout = %q, want the unquoted string", out)
	}
}
