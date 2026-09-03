package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
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

// A file body past what the CLI used to hold is SENT, not refused. The cap was
// the CLI's own — the instance has no such limit — and it was reported as a
// judgement on the file ("is that the file you meant?"). A file is streamed off
// disk now, so the only thing that can refuse it is the server.
func TestAPISendsABodyFileLargerThanTheStdinCap(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.json")
	const size = maxStdinBody + 1
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, dir, "api", "-d", "@"+path, "/api/v2/feature"); err != nil {
		t.Fatalf("a body file past the old cap was refused: %v", err)
	}
	req := e.only()
	if got := len(req.Body); got != size {
		t.Errorf("body = %d bytes, want %d — the whole file must reach the server", got, size)
	}
	// Streamed, not chunked: Content-Length is what lets the instance apply
	// its own ceiling before reading the body at all.
	if got := req.Header.Get("Content-Length"); got != strconv.Itoa(size) {
		t.Errorf("Content-Length = %q, want %d", got, size)
	}
}

// A multipart file part is streamed too, and arrives as a real form part — the
// filename and content type survive the segmenting.
func TestAPISendsAFormFileLargerThanTheStdinCap(t *testing.T) {
	const size = maxStdinBody + 1
	var (
		gotSize     int
		gotFilename string
		gotType     string
		gotField    string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("not a multipart request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			n, _ := io.Copy(io.Discard, part)
			if part.FormName() == "file" {
				gotSize, gotFilename = int(n), part.FileName()
				gotType = part.Header.Get("Content-Type")
			}
			if part.FormName() == "note" {
				gotField = "seen"
			}
			part.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	signInTo(t, server.URL)

	dir := t.TempDir()
	path := filepath.Join(dir, "quarterly.csv")
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, dir, "api", "-X", "POST", "/api/v2/file/upload/uploads",
		"-F", "file=@"+path, "-F", "note=draft"); err != nil {
		t.Fatalf("a form file past the old cap was refused: %v", err)
	}
	if gotSize != size {
		t.Errorf("file part = %d bytes, want %d", gotSize, size)
	}
	if gotFilename != "quarterly.csv" {
		t.Errorf("filename = %q", gotFilename)
	}
	if !strings.HasPrefix(gotType, "text/csv") {
		t.Errorf("Content-Type = %q, want the guessed type rather than octet-stream", gotType)
	}
	if gotField != "seen" {
		t.Error("the plain field beside the file was lost")
	}
}

// --retry re-sends the WHOLE body. The segments are re-opened per attempt, so
// the second request must be byte-identical to the first — a body that
// evaporated after attempt one would retry as an empty upload the server
// accepts.
func TestAPIRetryResendsAFileBodyInFull(t *testing.T) {
	e := newEchoServer(t)
	e.script = []echoedReply{{Status: 503}, {Status: 200}}
	signInTo(t, e.URL())
	dir := t.TempDir()
	path := filepath.Join(dir, "body.json")
	payload := strings.Repeat("a", 4096)
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, dir, "api", "-X", "POST", "--retry", "1", "-d", "@"+path, "/api/v2/feature"); err != nil {
		t.Fatalf("api: %v", err)
	}
	if len(e.requests) != 2 {
		t.Fatalf("served %d requests, want 2", len(e.requests))
	}
	for i, req := range e.requests {
		if req.Body != payload {
			t.Errorf("attempt %d sent %d bytes, want the full %d", i+1, len(req.Body), len(payload))
		}
	}
}

// A file that changes size between the stat and the send is a real hazard of
// streaming, and net/http refuses to send a body that disagrees with the
// Content-Length already written. The CLI must name the file and say what to
// do, rather than pass on "http: ContentLength=100 with Body length 40".
func TestAPISaysWhenAFileChangedSizeWhileItWasSent(t *testing.T) {
	e := newEchoServer(t)
	request := apiRequest{
		client:  api.New(e.URL(), "tok"),
		method:  http.MethodPost,
		path:    "/api/v2/feature",
		header:  http.Header{},
		body:    shrinkingBody{path: "growing.json", claimed: 4096, actual: 40},
		timeout: 10 * time.Second,
	}

	_, err := request.send(context.Background())
	if err == nil {
		t.Fatal("a short body was sent as if it were whole")
	}
	if !strings.Contains(err.Error(), "growing.json changed size while it was being sent") {
		t.Errorf("error does not name the file or the cause: %v", err)
	}
}

// shrinkingBody claims one length and yields another, which is exactly what a
// file being written to does between its stat and its send. Implementing
// paths() is what marks it as a body whose bytes came from a named file.
type shrinkingBody struct {
	path    string
	claimed int64
	actual  int
}

func (b shrinkingBody) Len() int64 { return b.claimed }

func (b shrinkingBody) Open() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(strings.Repeat("x", b.actual))), nil
}

func (b shrinkingBody) paths() []sizedPath {
	return []sizedPath{{path: b.path, size: b.claimed}}
}

// The same hazard with a real file on disk, which is where the two directions
// stop behaving alike.
//
// A file that SHRANK ends short of the Content-Length already written and fails
// at the transport, which is what this covers: a genuine *os.File, opened by
// fileBody, whose stat'ed size no longer matches what is there to read.
//
// ⚠️ This is a TOOLCHAIN-COMPATIBILITY CANARY, not an ordinary behaviour test.
// explainBodySizeChange recognises the transport failure by matching net/http's
// own wording ("ContentLength=") — a string, because the error is built with
// errors.New at the point of the write and there is no sentinel to compare
// against. A stdlib rewording therefore breaks nothing at build time; it makes
// the CLI silently fall back to emitting the raw transport error, on the exact
// path this branch exists to make truthful, and THIS TEST is the only thing
// that says so. If it goes red after a Go upgrade it is reporting a real
// regression in the message: fix the match in explainBodySizeChange. Do not
// skip it, and do not read it as flaky.
func TestAPISaysWhenARealFileShrankWhileItWasSent(t *testing.T) {
	e := newEchoServer(t)
	path := filepath.Join(t.TempDir(), "shrinking.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 40)), 0o600); err != nil {
		t.Fatal(err)
	}

	request := apiRequest{
		client: api.New(e.URL(), "tok"),
		method: http.MethodPost,
		path:   "/api/v2/feature",
		header: http.Header{},
		// The size a stat took before the file was truncated behind it.
		body:    fileBody{path: path, size: 4096},
		timeout: 10 * time.Second,
	}

	_, err := request.send(context.Background())
	if err == nil {
		t.Fatal("a short file was sent as if it were whole")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "changed size while it was being sent") {
		t.Errorf("error does not name the file or the cause: %v", err)
	}
}

// A file that GREW is the direction net/http does not catch: it writes exactly
// Content-Length bytes off a LimitReader, so the request is complete and valid
// on the wire and the server answers 200 on a PREFIX of the file. Only the
// re-stat after the response sees it.
//
// The handler drains the whole declared body BEFORE appending, so nothing here
// races the transport: by the time the file grows, the request is already
// finished and the only thing left that can notice is the re-stat.
func TestAPISaysWhenARealFileGrewWhileItWasSent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "growing.json")
	const sent = 1024
	if err := os.WriteFile(path, []byte(strings.Repeat("x", sent)), 0o600); err != nil {
		t.Fatal(err)
	}

	var served int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
		}
		if n != sent {
			t.Errorf("server read %d bytes, want the declared %d", n, sent)
		}
		// Only after the full declared body is in: this is the file continuing
		// to be written while the upload the server just accepted was in flight.
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Errorf("appending to the sent file: %v", err)
		} else {
			_, _ = f.WriteString(strings.Repeat("y", 2048))
			_ = f.Close()
		}
		served++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	signInTo(t, server.URL)

	stdout, err := runCLI(t, dir, "api", "-X", "POST", "-d", "@"+path, "/api/v2/feature")
	if err == nil {
		t.Fatal("a truncated upload exited zero")
	}
	if served != 1 {
		t.Fatalf("served %d requests, want 1", served)
	}
	for _, want := range []string{path, "changed size while it was being sent", "may have accepted the first 1024 bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	// The response was emitted BEFORE the failure: the request really did
	// succeed, and hiding what the server said would leave the caller unable to
	// find the half-written upload it just created.
	if !strings.Contains(stdout, `{"ok":true}`) {
		t.Errorf("the server's answer was swallowed: %q", stdout)
	}
}

// The same growth, answered with a 400. A file that shrank or grew mid-upload
// is a plausible CAUSE of the status the server just returned, so the re-stat
// has to run on a failed request too — otherwise the one command that could
// explain the 400 reports only the 400.
//
// Both halves must reach the caller: the "HTTP 400" line emit already wrote,
// and the truncation message. The trap this guards is that emit's error is
// errAlreadyReported, which suppresses printing — joining it would have taken
// the truncation message down with it.
func TestAPIReportsATruncatedUploadUnderAnHTTPError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "growing.json")
	const sent = 1024
	if err := os.WriteFile(path, []byte(strings.Repeat("x", sent)), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Errorf("appending to the sent file: %v", err)
		} else {
			_, _ = f.WriteString(strings.Repeat("y", 2048))
			_ = f.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"malformed json"}`)
	}))
	defer server.Close()
	signInTo(t, server.URL)

	stdout, err := runCLI(t, dir, "api", "-X", "POST", "-d", "@"+path, "/api/v2/feature")
	if err == nil {
		t.Fatal("a 400 on a truncated upload exited zero")
	}
	for _, want := range []string{path, "changed size while it was being sent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	if errors.Is(err, errAlreadyReported) {
		t.Error("the failure claims it has already been reported, so the truncation message is never printed")
	}
	if !strings.Contains(stdout, "malformed json") {
		t.Errorf("the server's error body was swallowed: %q", stdout)
	}
}

// --timeout bounds the whole request, the body going UP included, which is
// invisible until the upload is large: a big -F dies partway through the send
// with a message about a deadline and nothing about the file. The instance is
// not slow — the CLI stopped listening — and the remedy is not the obvious one,
// so it has to be named.
func TestAPISaysWhenTheUploadRanOutOfTimeRatherThanTheInstance(t *testing.T) {
	client := api.New("http://instance.invalid", "tok")
	client.HTTP = &http.Client{Transport: alwaysTimesOut{}}
	request := apiRequest{
		client:  client,
		method:  http.MethodPost,
		path:    "/api/v2/file/upload/uploads",
		header:  http.Header{},
		body:    namedBody{path: "big.bin", size: 8},
		timeout: 90 * time.Second,
	}

	_, err := request.send(context.Background())
	if err == nil {
		t.Fatal("a timed-out upload came back clean")
	}
	for _, want := range []string{"big.bin", "--timeout (1m30s)", "--timeout 0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	// The chain survives the rewrite: whether the write may have landed is a
	// question the callers ask of the error itself.
	if !api.IsTimeout(err) {
		t.Errorf("api.IsTimeout no longer sees the deadline through the message: %v", err)
	}
}

// alwaysTimesOut fails every request the way an expired deadline does;
// http.Client wraps it in the *url.Error a real one arrives as.
type alwaysTimesOut struct{}

func (alwaysTimesOut) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

// namedBody is a body whose bytes came from a file on disk, which is all the
// command can tell about one and all the two explainers need to know.
type namedBody struct {
	path string
	size int64
}

func (b namedBody) Len() int64 { return b.size }

func (b namedBody) Open() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(strings.Repeat("x", int(b.size)))), nil
}

func (b namedBody) paths() []sizedPath { return []sizedPath{{path: b.path, size: b.size}} }

// The same growth, through -F: a multipart body's file segments are re-stat'ed
// too, and the message names the one that changed rather than every path in the
// request.
func TestAPISaysWhichFormFileGrewWhileItWasSent(t *testing.T) {
	dir := t.TempDir()
	steady := filepath.Join(dir, "steady.csv")
	growing := filepath.Join(dir, "growing.csv")
	for _, p := range []string{steady, growing} {
		if err := os.WriteFile(p, []byte(strings.Repeat("a", 512)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		f, err := os.OpenFile(growing, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Errorf("appending to the sent file: %v", err)
		} else {
			_, _ = f.WriteString(strings.Repeat("b", 64))
			_ = f.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	signInTo(t, server.URL)

	_, err := runCLI(t, dir, "api", "-X", "POST", "/api/v2/file/upload/uploads",
		"-F", "one=@"+steady, "-F", "two=@"+growing)
	if err == nil {
		t.Fatal("a truncated form upload exited zero")
	}
	if !strings.Contains(err.Error(), growing) {
		t.Errorf("error does not name the file that changed: %v", err)
	}
	if strings.Contains(err.Error(), steady) {
		t.Errorf("error blames a file that did not change: %v", err)
	}
}

// A path that is readable but not a REGULAR file is not a mistake to refuse —
// it is `-d @<(gzip -c dump.sql)`, `-F file=@/dev/stdin`, a named pipe a
// producer is writing into. Those cannot be streamed (their stat size is not
// the size of what reading them yields) and cannot be re-opened for a retry, so
// they are read into memory and sent from there, exactly as `@-` is.
//
// The assertion is that the bytes ARRIVE: a check that only looked at the exit
// code would pass just as well on a request that went out empty, which is the
// failure this shape actually invites.
func TestAPISendsANonRegularFileBody(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mkfifo")
	}
	const payload = "a,b\n1,2\n"

	for _, tc := range []struct {
		name string
		args func(fifo string) []string
	}{
		{"data", func(fifo string) []string { return []string{"-d", "@" + fifo} }},
		{"form", func(fifo string) []string { return []string{"-F", "file=@" + fifo} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEchoServer(t)
			signInTo(t, e.URL())
			dir := t.TempDir()
			fifo := filepath.Join(dir, "pipe")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			// A FIFO's reader blocks until a writer opens it, so the content
			// has to be produced concurrently — which is exactly the situation
			// a caller is in when they point the CLI at one.
			written := make(chan error, 1)
			go func() {
				f, err := os.OpenFile(fifo, os.O_WRONLY, 0o600)
				if err != nil {
					written <- err
					return
				}
				_, err = io.WriteString(f, payload)
				written <- errors.Join(err, f.Close())
			}()

			args := append([]string{"api", "-X", "POST"}, tc.args(fifo)...)
			if _, err := runCLI(t, dir, append(args, "/api/v2/file/upload/uploads")...); err != nil {
				t.Fatalf("a readable FIFO was refused: %v", err)
			}
			if err := <-written; err != nil {
				t.Fatalf("writing the FIFO: %v", err)
			}
			if !strings.Contains(e.only().Body, payload) {
				t.Errorf("the FIFO's content never reached the instance: %q", e.only().Body)
			}
		})
	}
}

// A character device is the same shape, and the one a caller actually types
// (`-d @/dev/stdin`). /dev/null stands in for it: same mode, no unbounded read
// if the cap ever regresses, and an empty body is still a body that must be
// SENT rather than refused.
func TestAPISendsACharacterDeviceBody(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/null")
	}
	e := newEchoServer(t)
	signInTo(t, e.URL())

	if _, err := runCLI(t, t.TempDir(), "api", "-X", "POST", "-d", "@/dev/null", "/api/v2/feature"); err != nil {
		t.Fatalf("a character device was refused: %v", err)
	}
	if got := e.only().Body; got != "" {
		t.Errorf("/dev/null sent %q, want nothing at all", got)
	}
}

// The one cap left is on a PIPE, and the message has to say so: the limit is
// not the instance's and not a judgement on the content, and the remedy is a
// file rather than a smaller body.
func TestAPIRefusesAnOversizedPipedBody(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	withStdin(t, strings.Repeat("a", maxStdinBody+1))

	_, err := runCLI(t, t.TempDir(), "api", "-X", "POST", "-d", "@-", "/api/v2/feature")
	if err == nil {
		t.Fatal("an oversized piped body was accepted")
	}
	for _, want := range []string{"standard input", "16 MiB", "point -d at the path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// ...and a piped body exactly at the cap is not: an off-by-one here would
// refuse a legitimate payload with a message about a limit it did not exceed.
func TestAPIAcceptsAPipedBodyAtTheLimit(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	withStdin(t, strings.Repeat("a", maxStdinBody))

	if _, err := runCLI(t, t.TempDir(), "api", "-X", "POST", "-d", "@-", "/api/v2/feature"); err != nil {
		t.Fatalf("a piped body exactly at the limit was refused: %v", err)
	}
	if got := len(e.only().Body); got != maxStdinBody {
		t.Errorf("body = %d bytes, want %d", got, maxStdinBody)
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

// --- the body builder's own invariants ---------------------------------------

// A multipart body opens its file parts LAZILY — one at a time, closed at its
// own EOF — so the two things worth pinning are that a full read leaves nothing
// open, and that a part which cannot be opened at all still leaks nothing.
//
// Note where the failure now surfaces: at READ time rather than at Open. That
// is the deliberate cost of the laziness (checkFormPaths is what catches a bad
// path before the request is shaped), and a test that still expected an error
// from Open would be asserting the eager behaviour rather than this one.
//
// The count is taken from the process's own open descriptors, and the
// SUCCESSFUL case is checked first: it proves the counter can see handles at
// all, so the failing case's "unchanged" is evidence rather than a tautology.
func TestFormBodyClosesWhatItOpenedWhenAPartCannotBeOpened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/fd")
	}
	dir := t.TempDir()
	var specs []string
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = filepath.Join(dir, "part"+strconv.Itoa(i)+".csv")
		if err := os.WriteFile(paths[i], []byte("a,b\n1,2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, "file"+strconv.Itoa(i)+"=@"+paths[i])
	}
	body, _, err := buildForm(specs)
	if err != nil {
		t.Fatal(err)
	}

	// Each baseline is taken immediately before the read it belongs to. One
	// baseline for the whole test would be measuring across a window in which an
	// unrelated idle connection could close, which is a flake nobody would ever
	// reproduce.
	base := openDescriptorCount(t)
	rc, err := body.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Read one byte at a time, so at least one measurement lands while a part
	// is genuinely open. Reading the body whole would leave the count at zero
	// throughout and prove nothing about what laziness holds.
	held := 0
	single := make([]byte, 1)
	for {
		if n := openDescriptorCount(t) - base; n > held {
			held = n
		}
		if _, err := rc.Read(single); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read: %v", err)
		}
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if held != 1 {
		t.Fatalf("reading the body held %d descriptors at once, want exactly 1 — either the parts are still opened eagerly, or the count cannot see file handles at all",
			held)
	}
	if got := openDescriptorCount(t) - base; got != 0 {
		t.Errorf("a fully-read body left %d descriptors open — the last part was not closed at its EOF", got)
	}

	// Now the third part cannot be opened at all. The failure surfaces while
	// the body is being READ, and the parts read before it must already be
	// closed — without that, a --retry whose later attempt hits an unopenable
	// file leaks a handle per attempt, invisible until a long upload loop runs
	// out of descriptors.
	if err := os.Remove(paths[2]); err != nil {
		t.Fatal(err)
	}
	base = openDescriptorCount(t)
	rc, err = body.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := io.Copy(io.Discard, rc); err == nil {
		t.Fatal("reading a body whose third part is gone succeeded")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close after a failed read: %v", err)
	}
	if got := openDescriptorCount(t) - base; got != 0 {
		t.Errorf("a failed read leaked %d descriptors — the parts opened before the failure were not closed", got)
	}
}

// openDescriptorCount is how many file descriptors this process holds.
//
// /dev/fd is the one portable-enough view of that (a symlink to /proc/self/fd
// on Linux, an fdesc mount on macOS). Reading it opens a descriptor of its own,
// which is fine: every call pays the same one, so differences are still exact.
//
// Readdirnames rather than os.ReadDir: ReadDir stats every entry, and on macOS
// stat'ing the descriptor the listing itself is using fails with EBADF — the
// count would be unobtainable on the platform most of this is developed on.
func openDescriptorCount(t *testing.T) int {
	t.Helper()
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("cannot count open descriptors: %v", err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		t.Skipf("cannot count open descriptors: %v", err)
	}
	return len(names)
}

// A directory is the non-regular path a caller actually types by mistake, and
// the general refusal's advice ("pipe it in with @-") is useless for one.
func TestAPIRefusesADirectoryAsABody(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())
	dir := t.TempDir()
	inner := filepath.Join(dir, "uploads")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"data", []string{"-d", "@" + inner}},
		{"form", []string{"-F", "file=@" + inner}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"api", "-X", "POST"}, tc.args...)
			_, err := runCLI(t, dir, append(args, "/api/v2/file/upload/uploads")...)
			if err == nil {
				t.Fatal("a directory was accepted as a body")
			}
			for _, want := range []string{"is a directory, not a file", "point at a file inside it"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not say %q: %v", want, err)
				}
			}
			if len(e.requests) != 0 {
				t.Errorf("a request went out anyway: %+v", e.requests)
			}
		})
	}
}

// The negative control on the translation: explainBodySizeChange is narrowed by
// the BODY's type, not only by the error text, so a ContentLength mismatch on a
// body with no file behind it must come back exactly as it was. Rewriting that
// one into "changed size while it was being sent" would name a file that does
// not exist and send the reader looking for it.
func TestExplainBodySizeChangeLeavesABytesBodyAlone(t *testing.T) {
	err := errors.New("http: ContentLength=10 with Body length 3")
	got := explainBodySizeChange(err, api.BytesBody("abc"))
	if got != err { //nolint:errorlint // identity is the assertion
		t.Errorf("a bytes body's transport error was rewritten: %v", got)
	}
}

// The segmented body is hand-built framing around streamed file bytes, so the
// one thing it can get wrong is the framing ORDER — a part header flushed after
// the bytes it describes, a boundary written twice, a missing CRLF. Pinning it
// against mime/multipart's own output for the same inputs is what makes that a
// test failure rather than a server-side parse error nobody can read.
//
// A zero-byte file part is in the list on purpose: it is the case where a
// segment carries no bytes at all and the two literals around it end up
// adjacent.
func TestFormBodyMatchesTheStdlibMultipartWriterByteForByte(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "rows.csv")
	empty := filepath.Join(dir, "empty.csv")
	payload := []byte("a,b\n1,2\n")
	if err := os.WriteFile(full, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	body, contentType, err := buildForm([]string{
		"first=one", "rows=@" + full, "second=two", "blank=@" + empty, "third=three",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("content type %q: %v", contentType, err)
	}

	rc, err := body.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != body.Len() {
		t.Errorf("body is %d bytes but Len() says %d — Content-Length would be wrong", len(got), body.Len())
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.SetBoundary(params["boundary"]); err != nil {
		t.Fatal(err)
	}
	filePart := func(name, filename string, content []byte) {
		t.Helper()
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="`+name+`"; filename="`+filename+`"`)
		header.Set("Content-Type", mime.TypeByExtension(filepath.Ext(filename)))
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteField("first", "one"); err != nil {
		t.Fatal(err)
	}
	filePart("rows", "rows.csv", payload)
	if err := writer.WriteField("second", "two"); err != nil {
		t.Fatal(err)
	}
	filePart("blank", "empty.csv", nil)
	if err := writer.WriteField("third", "three"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, buf.Bytes()) {
		t.Errorf("segmented body differs from mime/multipart's own framing.\n got: %q\nwant: %q", got, buf.Bytes())
	}
}

// A regular file whose stat size is 0 but which yields bytes when read must
// send those bytes.
//
// The stat size of a regular file is normally the whole basis for streaming it
// straight off disk, and for 0 it is the one value that can be a lie: procfs,
// sysfs and cgroup files report no size and read out content anyway, and so do
// some FUSE mounts. Trusting it produced the quietest possible failure — an
// EMPTY request body, no Content-Length mismatch to fail on, a re-stat
// comparing 0 against 0, and a zero exit — so classifyBodyPath sends a
// zero-sized regular file down the buffered path instead.
//
// /proc is not on darwin and cannot be fabricated portably, so the assertion
// is on the classification decision, which is where the choice is actually
// made. The empty-file case is asserted alongside it, because that one must
// keep behaving exactly as it did.
func TestAPIReadsARegularFileWhoseStatSizeIsZero(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sized := filepath.Join(dir, "sized.json")
	if err := os.WriteFile(sized, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// A zero-length regular file is classified as unseekable, so its bytes
	// decide rather than its stat. It really is empty, so the outcome for it
	// is unchanged — an empty body — but it now gets there by being read.
	kind, _, err := classifyBodyPath("-d @"+empty, empty)
	if err != nil {
		t.Fatalf("classify empty file: %v", err)
	}
	if kind != pathUnseekable {
		t.Errorf("a regular file with stat size 0 was classified %v, want pathUnseekable — its stat size cannot be trusted, so its bytes have to be read", kind)
	}

	// A file with a real size keeps streaming off disk, which is the whole
	// point of the branch and must not regress.
	kind, _, err = classifyBodyPath("-d @"+sized, sized)
	if err != nil {
		t.Fatalf("classify sized file: %v", err)
	}
	if kind != pathRegular {
		t.Errorf("a regular file with a non-zero stat size was classified %v, want pathRegular — it must still stream off disk", kind)
	}

	// End to end: the empty file still sends an empty body, and the request
	// succeeds rather than being refused for having no size.
	body, err := fileSource("-d @"+empty, "-d", empty)
	if err != nil {
		t.Fatalf("fileSource on an empty file: %v", err)
	}
	if got := body.Len(); got != 0 {
		t.Errorf("empty file body length = %d, want 0", got)
	}
}

// A file rewritten IN PLACE to the same length is reported, not passed over.
//
// This is the quietest member of the stat-then-send family and the only one
// that clears both existing guards: the transport wrote exactly
// Content-Length bytes, so there is no mismatch for net/http to raise, and a
// size-only re-stat finds precisely the number it measured. What reached the
// instance is part of the old file and part of the new one, stored as a
// finished upload, and before the modification time was compared the command
// exited zero and said nothing at all.
func TestCheckFilesUnchangedSpotsASameSizeRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swapped.json")
	original := []byte(strings.Repeat("a", 32))
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	body := fileBody{path: path, size: fi.Size(), modTime: fi.ModTime()}

	// Unchanged: nothing to report.
	if err := checkFilesUnchanged(body); err != nil {
		t.Fatalf("an untouched file was reported as changed: %v", err)
	}

	// Rewritten to the SAME length, with a modification time the filesystem
	// can actually distinguish from the first write.
	replacement := []byte(strings.Repeat("b", len(original)))
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	err = checkFilesUnchanged(body)
	if err == nil {
		t.Fatal("a same-size in-place rewrite was not reported — the upload is a mix of both versions and the command would have exited zero")
	}
	if !strings.Contains(err.Error(), "was modified while it was being sent") {
		t.Errorf("message does not name what was detected: %v", err)
	}
	// It must NOT claim the size changed, because it did not.
	if strings.Contains(err.Error(), "changed size") {
		t.Errorf("a same-size rewrite was reported as a size change: %v", err)
	}

	// A body carrying no build-time modTime knows nothing about this and must
	// claim nothing: "not known" is not "changed".
	if err := checkFilesUnchanged(fileBody{path: path, size: fi.Size()}); err != nil {
		t.Errorf("a body with no recorded modification time claimed a change it cannot know about: %v", err)
	}
}
