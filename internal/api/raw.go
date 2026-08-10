package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The raw request path, which backs `ronja api`.
//
// Everything else in this package MIRRORS an endpoint: a Go type per request and
// response, kept in step with a handler by hand. This one deliberately mirrors
// nothing. It carries transport and auth — base URL, bearer token, user agent,
// timeout — and hands the bytes back undecoded, so a new endpoint needs no
// change here and cannot drift from anything.
//
// Note the two differences from Do:
//
//   - The path is relative to the instance ROOT, not to /api/v2. `ronja api` is
//     the general runner, and the agent-readable docs surface (/llms.txt,
//     /docs/api/...) does not live under the API prefix.
//   - The response is not decoded, or even read. A query result can be large,
//     and the command's job is to stream it to stdout rather than to hold it.

// DefaultRawTimeout bounds one raw call when the caller names no deadline.
//
// The slow bound rather than the read bound: an arbitrary POST can be anything,
// including the endpoints that legitimately do real work server-side, and
// killing one of those at 30s reports a healthy instance as a broken one.
//
// It is only a DEFAULT, for the same reason DefaultQueryTimeout is: `ronja api`
// can be pointed at an endpoint that runs for minutes, and a bound nobody can
// move is a bound that eventually kills a healthy request. `--timeout` moves it,
// including past the client's own ceiling — see httpFor.
const DefaultRawTimeout = slowRequestTimeout

// RawResponse is one undecoded HTTP response. The caller owns Body and MUST
// close it — closing is also what releases the request's deadline.
//
// Header was added back when a caller finally needed it: `ronja api --retry`
// honours Retry-After, and a retry that ignores the interval the server just
// named is the one behaviour a rate limiter is entitled to treat as abuse.
// Still deliberately narrow — everything else about the response is the body.
type RawResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// DoRaw issues an arbitrary request against a path relative to the instance
// root and returns the response without reading it.
//
// header is applied verbatim; the defaults below fill in only what it does not
// already carry, so a caller can override any of them — including Authorization,
// which is how someone tests a different credential without re-logging-in.
//
// timeout bounds the whole call, the response body INCLUDED — the deadline
// rides on Body.Close, not on this function's return. Zero or less means no
// client-side deadline at all, which the command surfaces as `--timeout 0`.
func (c *Client) DoRaw(ctx context.Context, method, path string, header http.Header, body []byte, timeout time.Duration) (*RawResponse, error) {
	// NOT deferred: the deadline has to outlive this function, because the
	// caller is about to stream a body that is still in flight. Cancelling on
	// return would truncate every response larger than one buffer. The
	// cancellation rides on Body.Close instead.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		// No deadline. Still a DERIVED cancellable context, so Body.Close
		// releases the request rather than leaking it — and note that this case
		// cannot be written as context.WithTimeout(ctx, 0), which produces an
		// already-expired context, i.e. the exact opposite of "wait as long as
		// it takes".
		ctx, cancel = context.WithCancel(ctx)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	url := c.BaseURL + "/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build request: %w", err)
	}
	if header != nil {
		req.Header = header.Clone()
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", UserAgent)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	// Host is the one header net/http does not take from the header map: the
	// request line and the Host line are written from req.Host, and a "Host"
	// key in Header is silently DROPPED. Honouring it explicitly is what makes
	// -H "Host: ..." mean what it plainly says, rather than doing nothing at
	// all — which is the worst of the three available behaviours.
	if host := req.Header.Get("Host"); host != "" {
		req.Host = host
	}

	resp, err := c.httpFor(timeout).Do(req)
	if err != nil {
		cancel()
		// The error carries the METHOD and URL and nothing else. Headers — the
		// bearer token among them — are never part of a url.Error, and must
		// never be added here.
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	return &RawResponse{
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   cancelOnClose{ReadCloser: resp.Body, cancel: cancel},
	}, nil
}

// cancelOnClose ties a request's context cancellation to the response body's
// lifetime, so the deadline is released exactly when the caller is done reading
// rather than when DoRaw returns.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
