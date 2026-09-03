// Package api is a thin HTTP client for Ronja's /api/v2 surface.
//
// It is deliberately hand-rolled rather than generated from the OpenAPI spec:
// the CLI touches a handful of endpoints, and a generated client would drag in
// a code-generation step and a dependency for no benefit at this size.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UserAgent identifies the CLI in server logs.
const UserAgent = "ronja-cli"

// maxErrorBody bounds how much of an error response we read before giving up.
// A misconfigured URL pointed at something that is not Ronja can otherwise
// stream megabytes of HTML into an error message.
const maxErrorBody = 8 << 10

// maxDocBody caps the agent-readable docs surface (/llms.txt and friends).
// Generous for the real thing, small enough that a misaimed --url fails fast.
const maxDocBody = 2 << 20

// maxDiscardBody bounds the drain of a response nobody decodes. Draining is
// what returns the connection to the pool; bounding it is what stops a
// misaimed --url making that drain the request.
const maxDiscardBody = 64 << 10

// Request timeouts. The split is the point.
//
// http.Client.Timeout is a CEILING, not a policy: a per-request
// context.WithTimeout can only ever SHORTEN a deadline, never push one past the
// client's. A single 30s client timeout therefore capped every call, including
// the two that legitimately take longer — a whole-folder validate and a file
// PUT, both of which re-derive a workflow's bindings server-side — and killed
// them with a message about nothing.
//
// So the client timeout is now the longest anything may take, and the actual
// policy lives per call: quick reads keep the 30s they always had, and the slow
// pair get their own deadline. What the ceiling still buys is the case a
// context cannot cover — a host that accepts the connection and then says
// nothing at all.
const (
	clientTimeout      = 120 * time.Second
	requestTimeout     = 30 * time.Second
	slowRequestTimeout = 120 * time.Second
)

// Default device-flow poll timing. The server advertises its own interval and
// enforces it, so these are a floor, a backoff step and a ceiling rather than
// the primary cadence.
const (
	DefaultMinPollInterval = 5 * time.Second
	DefaultPollBackoff     = 2 * time.Second
	DefaultMaxPollInterval = 20 * time.Second
	// DefaultMaxPollFailureWindow bounds an UNBROKEN RUN of transient poll
	// failures (a dropped connection, a 429, a 5xx from a pod restarting
	// mid-deploy) by ELAPSED TIME rather than by a count.
	//
	// A count was the wrong unit: it silently means "count x interval", so the
	// real tolerance moved whenever the cadence did. Five retries at the 5s
	// floor is ~25 seconds, which is less than an ordinary rolling deploy takes
	// — a login would die on a perfectly healthy instance mid-release. Elapsed
	// time says what is actually meant, and stays honest under backoff.
	//
	// Generous is nearly free here: a login has ~10 minutes of life, and the
	// flow deadline bounds total wall time independently of this. What this
	// still rules out is the case it exists for — a permanently broken instance
	// hidden behind a silent loop for the full ten minutes.
	DefaultMaxPollFailureWindow = 90 * time.Second
	// DefaultRateLimitBackoff is added to the interval on a 429, which is the
	// global rate limiter answering. It is the one transient failure where
	// retrying at the same cadence is actively wrong: the server is saying
	// there is too much traffic, and an unchanged cadence is a promise to keep
	// producing exactly as much.
	DefaultRateLimitBackoff = 5 * time.Second
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	// MinPollInterval floors the cadence PollDevice uses, so a server that
	// advertises nothing (or zero) still gets polled politely.
	MinPollInterval time.Duration
	// PollBackoff is added to the interval each time the server answers
	// slow_down.
	PollBackoff time.Duration
	// MaxPollInterval caps that backoff, so a run of slow_down answers cannot
	// stretch the cadence past the point where the flow expires unpolled.
	MaxPollInterval time.Duration
	// MaxPollFailureWindow bounds an unbroken run of transient failures by
	// elapsed time before PollDevice gives up. See isTransient.
	MaxPollFailureWindow time.Duration
	// RateLimitBackoff is added to the interval on a 429.
	RateLimitBackoff time.Duration

	// TableBuild is the schedule WaitForTableBuild polls on. Grouped rather than
	// flattened alongside the device-flow knobs above, because "MaxInterval"
	// belonging to one loop or the other is not something a reader should have
	// to infer from a comment.
	TableBuild TableBuildPoll
}

func New(baseURL, token string) *Client {
	base := strings.TrimRight(baseURL, "/")
	return &Client{
		BaseURL: base,
		Token:   token,
		HTTP: &http.Client{
			Timeout:       clientTimeout,
			CheckRedirect: redirectGuard(base),
		},
		MinPollInterval: DefaultMinPollInterval,
		PollBackoff:     DefaultPollBackoff,
		MaxPollInterval: DefaultMaxPollInterval,

		MaxPollFailureWindow: DefaultMaxPollFailureWindow,
		RateLimitBackoff:     DefaultRateLimitBackoff,

		TableBuild: defaultTableBuildPoll(),
	}
}

// maxRedirects is the hop limit the guard below enforces. Supplying a
// CheckRedirect REPLACES net/http's default policy, and the default policy is
// the only thing that stops a redirect loop after ten hops — so a guard that
// only checks the origin would happily follow a same-origin loop forever.
const maxRedirects = 10

// redirectGuard refuses any redirect that leaves the instance signed in to.
//
// net/http drops the Authorization header on a redirect only when the HOSTNAME
// changes, and on nothing else. A 302 to http://<same-host>:9999 keeps it, and
// so does a downgrade from https to http on the same name — either one hands a
// 90-day PAT to whatever is listening there. The instance does not have to be
// hostile for that to happen, only wrong (a misconfigured proxy is enough).
//
// So the whole ORIGIN is compared — scheme, host and port — against the base
// URL the credential belongs to, and anything else is refused rather than
// followed with the credential attached. Same-origin redirects still work,
// which is what an instance behind a path-rewriting proxy actually emits.
func redirectGuard(baseURL string) func(*http.Request, []*http.Request) error {
	base, err := url.Parse(baseURL)
	if err != nil {
		// Nothing to compare against. Refusing every redirect is the only safe
		// reading: forwarding on a guess is exactly what this exists to stop.
		return func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("refusing to follow the redirect to %s: the instance URL %q could not be parsed, so the credential is not forwarded",
				req.URL.Redacted(), baseURL)
		}
	}
	want := originOf(base)
	return func(req *http.Request, via []*http.Request) error {
		if got := originOf(req.URL); got != want {
			return fmt.Errorf("refusing to follow a redirect from %s to %s — that is a different origin, and the credential is never forwarded off the instance you signed in to",
				want, got)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}

// originOf renders scheme://host:port for comparison.
//
// Case is folded because scheme and host are case-insensitive, and an
// explicitly written DEFAULT port is folded onto the implicit form because
// "https://host:443" and "https://host" are the same origin — a caller who
// typed one should not be refused for reaching the other. Nothing else is
// normalised: a differing non-default port is precisely the case the guard is
// here for.
func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		host = strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		host = strings.TrimSuffix(host, ":443")
	}
	return scheme + "://" + host
}

// Error is a non-2xx response. Code carries the machine-readable `error` field
// when the server sent one — the CLI branches on it for the device-flow poll,
// where "authorization_pending" is an expected, non-fatal answer.
type Error struct {
	Status int
	Code   string
	Body   string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
	}
	if e.Body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// CodeOf extracts the server's machine-readable error code, or "" if the error
// was not an HTTP error.
func CodeOf(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// StatusOf extracts the HTTP status, or 0 if the error was not an HTTP error.
func StatusOf(err error) int {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// Unanswered reports that the instance never delivered a verdict — as opposed
// to delivering one the caller does not like.
//
// The distinction is who a message may blame. A 400 is the server having
// considered the request and refused it, so the refusal is about the request; a
// connection that never landed, a 429, or any 5xx is the instance not
// answering, and a CLI that renders those as "your files are wrong" attributes
// an outage to the author.
//
// A decode failure and a build-request failure are deliberately NOT included:
// both mean an answer arrived (or that we never got as far as asking), and
// neither is an outage.
//
// Neither is a CANCELLED context, and that one is not a technicality. root.go
// installs signal.NotifyContext, so Ctrl-C during a request surfaces here as a
// *url.Error wrapping context.Canceled — which the url.Error arm below would
// otherwise read as an outage, and a caller that warns-and-continues on an
// outage would then treat "the reader stopped this" as permission to carry on
// and write files.
//
// A DEADLINE is excluded for the same reason in reverse: the request stopped at
// OUR end, not the instance's, and IsTimeout is the separate question a caller
// asks about a write that may have landed. BOTH deadline shapes are excluded,
// which is why the test is IsTimeout rather than a bare
// errors.Is(context.DeadlineExceeded): the http.Client's own Timeout does not
// produce that sentinel at all, only a *url.Error that reports Timeout() — so
// checking the sentinel alone excluded the per-request deadline and let the
// client ceiling through the url.Error arm below as an outage, which is the
// opposite verdict on the same event.
//
// Separate from retryableStatus and the device flow's isTransient, which answer
// "should I try again" — a different question with a deliberately different
// membership.
func Unanswered(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || IsTimeout(err) {
		return false
	}
	if status := StatusOf(err); status != 0 {
		return status == http.StatusTooManyRequests || status >= 500
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// IsTimeout reports a request that died on a DEADLINE rather than on an answer.
//
// The distinction is what makes a write recoverable: a 4xx means the server
// considered the request and refused it, while a timeout means we stopped
// listening — the write may well have committed, and the caller has to go and
// look rather than assume either way.
//
// Both forms are checked because they arrive differently: a per-request context
// deadline surfaces as context.DeadlineExceeded, while the http.Client's own
// Timeout surfaces as a *url.Error that merely reports Timeout() — and which of
// the two fired is not something a caller should have to reason about.
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Do issues a request against /api/v2 and decodes a JSON response into out.
// Pass a nil body to send none, and a nil out to discard the response.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	return c.do(ctx, requestTimeout, method, path, body, out)
}

// doSlow is Do for the calls that legitimately take longer than a read: they do
// real work server-side, and dying on a deadline that fits a GET reports a
// healthy instance as a broken one.
func (c *Client) doSlow(ctx context.Context, method, path string, body, out any) error {
	return c.do(ctx, slowRequestTimeout, method, path, body, out)
}

// httpFor picks the http.Client for a request bounded by timeout.
//
// This exists because of the trap already documented on clientTimeout above,
// seen from the other side: http.Client.Timeout is a CEILING, and a per-request
// context can only ever SHORTEN a deadline. So a caller asking for LONGER than
// the ceiling — a query routed to Batch compute, which is minutes by design —
// would be silently capped at the ceiling and killed with a message about
// nothing. That is the same bug the split timeouts fixed for the 30s read
// bound, one level up.
//
// Within the ceiling, the shared client is used unchanged. Beyond it (or with
// no deadline at all), a SHALLOW COPY with Timeout cleared leaves the request's
// own context as the only bound. The copy shares the Transport, so the
// connection pool is not duplicated — only the policy field differs. It also
// carries CheckRedirect across, which is load-bearing: the redirect guard is
// what keeps the credential on the instance it belongs to, and a copy that lost
// it would be a hole opened by a timeout value.
func (c *Client) httpFor(timeout time.Duration) *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	if c.HTTP.Timeout > 0 && (timeout <= 0 || timeout > c.HTTP.Timeout) {
		unbounded := *c.HTTP
		unbounded.Timeout = 0
		return &unbounded
	}
	return c.HTTP
}

// do is the request path. The deadline is applied with context.WithTimeout,
// which takes the EARLIER of the two when the caller already has one — so a
// command that bounds its own work keeps that bound rather than having it
// silently extended here.
//
// A timeout of zero or less means NO client-side deadline, which is only ever
// asked for explicitly (`ronja query --timeout 0`). Note that it cannot be
// expressed by passing 0 to context.WithTimeout — that produces a context which
// is already expired, i.e. the exact opposite.
func (c *Client) do(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	url := c.BaseURL + "/api/v2/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpFor(timeout).Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseError(resp)
	}
	if out == nil {
		// DRAINED, not just closed. net/http only returns a connection to the
		// pool once its body has been read to EOF, so discarding a response by
		// closing it unread costs a fresh TCP+TLS handshake on the next call —
		// which the build poll loop makes hundreds of. Bounded for the same
		// reason maxDocBody is: --url can point at anything.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardBody))
		return nil
	}
	// Deliberately NOT capped, unlike GetText's maxDocBody. That cap protects a
	// read whose real content is a few KB; this one decodes the query envelope,
	// whose `result` field is a CSV of up to the server's 100 000-row ceiling —
	// a limit small enough to catch a misaimed --url would truncate a correct
	// answer into a decode error. A non-JSON body fails on its first byte
	// anyway, which is the case a cap would otherwise be for.
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s: %w", url, err)
	}
	return nil
}

// GetText fetches a path relative to the instance ROOT (not /api/v2) and
// returns it verbatim. The agent-readable docs surface — /llms.txt,
// /docs/api/endpoints.md, /docs/api/skills.md — lives there and is served
// unauthenticated, so this deliberately does not require a token.
func (c *Client) GetText(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	url := c.BaseURL + "/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/markdown, text/plain")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", parseError(resp)
	}
	// Capped: --url can point anywhere, and a wrong host (a CDN, a login page,
	// a file server) should fail as oversized rather than stream unbounded
	// content into memory. The real /llms.txt is a few KB.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBody))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", url, err)
	}
	if len(body) == maxDocBody {
		return "", fmt.Errorf("%s returned more than %d bytes — is that URL a Ronja instance?",
			url, maxDocBody)
	}
	return string(body), nil
}

// parseError turns a non-2xx response into an *Error, pulling out the `error`
// field when the body is the JSON shape the API uses.
func parseError(resp *http.Response) error {
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	// A body that stopped mid-stream is not the server's message, and presenting
	// the prefix as one would quote a sentence the server never finished — and
	// leave the machine-readable `error` field silently absent, which is what the
	// device-flow poll branches on. Said out loud, with whatever did arrive.
	if readErr != nil {
		apiErr := &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
		apiErr.Body = strings.TrimSpace(fmt.Sprintf("%s (the response body was truncated: %v)", apiErr.Body, readErr))
		return apiErr
	}
	return errorFrom(resp.StatusCode, raw)
}

// errorFrom builds an *Error from a status and a body that has ALREADY been
// read.
//
// Split out of parseError for the callers that cannot hand over an
// *http.Response: DoRaw returns the body undecoded, so a caller that reads it
// itself — the preview call, which needs the 200 bytes verbatim and therefore
// reads every status the same way — would otherwise have to re-derive the
// `error`-field extraction, and a second copy of it is a second opinion about
// what the server said.
func errorFrom(status int, raw []byte) *Error {
	apiErr := &Error{Status: status, Body: strings.TrimSpace(string(raw))}
	var shape struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &shape); err == nil && shape.Error != "" {
		apiErr.Code = shape.Error
	} else {
		// The API also returns a bare JSON string for some errors.
		var plain string
		if err := json.Unmarshal(raw, &plain); err == nil && plain != "" {
			apiErr.Body = plain
		}
	}
	return apiErr
}
