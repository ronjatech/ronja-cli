package commands

import (
	"context"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The flags that turned `ronja api` from a request runner into something you
// can build with: --jq, -F, --out, --retry, --fail-on-error and --wait-until.
//
// What is worth pinning is not that they work once, but the rules that are
// invisible from the output and expensive to get wrong: that a wait never
// re-sends a POST, that a filtered response is the one the condition matched,
// that --out never leaves a world-readable file, and that a retry is not
// applied to a 4xx.

// --- --jq -------------------------------------------------------------------

func TestAPIJQFiltersTheResponse(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"items":[{"id":"wf-1"},{"id":"wf-2"}]}`
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "--jq", ".items[].id", "/api/v2/workflow")
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if out != "\"wf-1\"\n\"wf-2\"\n" {
		t.Errorf("stdout = %q", out)
	}
}

// The form the whole flag exists for: $(...) into another command. A quoted
// "wf-1" substituted into a path is a path that does not exist.
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

// A bad expression must cost nothing. Compiling after the round trip would
// spend a request — and with --wait-until, a whole poll — to report a typo.
func TestAPIRefusesABadJQExpressionBeforeSending(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "--jq", ".items[", "/api/v2/workflow")
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
	// The filename has to survive: an upload handler stores it as the file's
	// name, and a part without one is a plain field most servers ignore.
	if !strings.Contains(req.Body, `filename="quarterly.pdf"`) {
		t.Errorf("body does not carry the filename:\n%s", req.Body)
	}
	// A real content type, not multipart's hardcoded octet-stream — Ronja
	// records it on the file row, and octet-stream costs preview and text
	// extraction.
	if !strings.Contains(req.Body, "Content-Type: application/pdf") {
		t.Errorf("body does not carry a guessed content type:\n%s", req.Body)
	}
	if !strings.Contains(req.Body, "draft") {
		t.Errorf("body is missing the literal field:\n%s", req.Body)
	}
}

// A missing file must be caught before anything is sent. Otherwise --retry
// spends its whole budget on a request that could never have succeeded.
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

// A pipe can be drained once, so a second reader gets EOF and its part goes out
// EMPTY — accepted by the server, discovered much later. Refusing is the only
// point at which it is diagnosable.
func TestAPIRefusesTwoFormFieldsReadingStdin(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "-X", "POST", "/api/v2/file/upload/x",
		"-F", "one=@-", "-F", "two=@-")
	if err == nil {
		t.Fatal("two -F fields both reading stdin were accepted")
	}
	// Both halves have to be named, or the caller cannot see which pair clashed.
	if !strings.Contains(err.Error(), "one=@-") || !strings.Contains(err.Error(), "two=@-") {
		t.Errorf("the error does not name both readers: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// A caller-supplied multipart Content-Type names a boundary the body does not
// use, so the server parses ZERO parts and reports success. The subtype is
// honoured; the boundary is replaced with the real one.
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
	// The declared boundary must be the one the body actually uses, which is
	// the whole point — assert it against the body rather than just against
	// the string "WRONG".
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", ct, err)
	}
	if b := params["boundary"]; b == "" || !strings.Contains(req.Body, b) {
		t.Errorf("declared boundary %q does not appear in the body", b)
	}
}

// A non-multipart Content-Type alongside -F is not an override, it is a
// contradiction: the body IS multipart, and honouring the header has the server
// parse it as something else.
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

// A filename carrying a quote must not be able to close the Content-Disposition
// header early and have the rest read as further headers.
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
	// stdout stays empty, mirroring `query --out`: the file IS the answer, and
	// a duplicate on stdout would land in whatever a pipeline is parsing.
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

// --out overwriting a pre-existing world-readable file must not inherit its
// mode. os.WriteFile would; the temp-file-and-rename does not.
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

// Both flags at once: the file gets the whole answer, stdout gets the field.
// That is what each of them plainly means, so combining them must not make
// either one lie.
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

// The trap: a failed query is HTTP 200 with `error` set, so a plain runner
// exits zero on something that never ran.
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
	// Still printed: the body is the most useful thing the server said, and
	// swallowing it makes every failure a second round trip.
	if !strings.Contains(out, "Binder Error") {
		t.Errorf("the body was swallowed: %q", out)
	}
}

// Without the flag the behaviour is unchanged — this is opt-in precisely
// because reading inside the body means holding all of it.
func TestAPIWithoutFailOnErrorA200EnvelopeStillExitsZero(t *testing.T) {
	e := newEchoServer(t)
	e.body = `{"error":"nope"}`
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "/api/v2/thing"); err != nil {
		t.Fatalf("api: %v", err)
	}
}

// A CSV or a zip has no envelope and must never be turned into a failure by a
// flag that looks for one.
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
	// Named by the server so the retry does not sleep a real second.
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

// A 4xx is the server saying the REQUEST was wrong. Repeating it just spends
// the budget to be told so again.
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
		// A DIFFERENT terminal answer, so a re-fetch after the condition held
		// is visible in stdout as well as in the request count. Without it the
		// repeating last entry would make a bug indistinguishable from correct
		// behaviour.
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
	// re-fetch — otherwise the printed status can differ from the one that
	// ended the wait.
	if out != "success\n" {
		t.Errorf("stdout = %q, want the matching response", out)
	}
	if len(e.requests) != 3 {
		t.Errorf("made %d requests, want 3", len(e.requests))
	}
}

// Re-sending a POST once per poll would create one resource per attempt,
// silently, for the whole --wait-timeout.
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
	// "context deadline exceeded" after a silent wait is the least useful thing
	// this could report; the condition is what the reader needs.
	if !strings.Contains(err.Error(), `.status == "done"`) {
		t.Errorf("the timeout does not name the condition: %v", err)
	}
}

// A 404 on the polled path is a typo, not "not ready yet". Looping on it for
// the whole timeout would report a wrong path as a slow one.
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

// jq truthiness is the thing people are wrong about: an empty array is TRUE.
// Pinned because a condition that fires immediately looks like success.
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

// A condition that produces NO OUTPUT AT ALL must not end the wait. This is
// the rule vacuous truth would break, and the case that reaches it is a filter
// over a stream rather than a comparison: `select(...)` that matches nothing
// yields zero results, not false. Treating that as satisfied would stop polling
// the instant an endpoint answered with a shape nobody expected.
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

// The neighbouring case, so the two are not confused: a comparison over an
// ABSENT field yields one output, `false` — not zero. Both must keep waiting,
// but they get there by different rules.
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

// A zero or already-elapsed duration makes both select cases ready at once, and
// select picks between ready cases at RANDOM. A cancelled context has to win
// every time, not half the time — the failure mode otherwise is a Ctrl-C (or a
// --wait-timeout) that is honoured in most runs and silently ignored in the
// rest.
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
