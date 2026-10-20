package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/jqf"
)

// What `ronja api` does with a response: send it, judge it, and emit it. No
// behaviour here knows anything about a specific endpoint.

// maxResponseBuffer bounds a response the command has to hold (--jq,
// --wait-until, --fail-on-error); the default path and --out stream, so this can
// fail loudly rather than truncate.
const maxResponseBuffer = 64 << 20

// jqFilterTimeout is the only time bound on one filter application:
// cmd.Context() has no deadline and --timeout bounds only the request. ⚠️ Like
// jqf's budgets it is checked between instructions, so it cannot cut one builtin
// short (jqf's join residual); it fails rather than truncating.
const jqFilterTimeout = 10 * time.Second

// retryBaseDelay is the first pause before a retried attempt; it doubles up to
// retryMaxDelay.
const (
	retryBaseDelay = 1 * time.Second
	retryMaxDelay  = 30 * time.Second
)

// DefaultWaitInterval is the cadence --wait-until polls at. The interval is
// exact and never backs off, so a named --wait-interval is honoured;
// --wait-timeout bounds the total cost.
const (
	DefaultWaitInterval = 3 * time.Second
	DefaultWaitTimeout  = 5 * time.Minute
)

// apiRequest is one fully-resolved, replayable request. The body is a
// BodySource, not a reader, because --retry and --wait-until send it more than
// once: a consumed reader would silently send an empty body the second time.
type apiRequest struct {
	client  *api.Client
	method  string
	path    string
	header  http.Header
	body    api.BodySource
	timeout time.Duration
	retries int
}

// send issues the request, retrying only 429 and 5xx up to r.retries times.
// Transport failures are not retried: a connection that dropped mid-request may
// have delivered it. The caller must close the returned body.
func (r apiRequest) send(ctx context.Context) (*api.RawResponse, error) {
	var lastStatus int
	for attempt := 0; ; attempt++ {
		resp, err := r.client.DoRaw(ctx, r.method, r.path, r.header, r.body, r.timeout)
		if err != nil {
			if timeoutErr := explainUploadTimeout(err, r.body, r.timeout); timeoutErr != nil {
				return nil, timeoutErr
			}
			return nil, explainBodySizeChange(err, r.body)
		}
		if attempt >= r.retries || !retryableStatus(resp.Status) {
			return resp, nil
		}
		lastStatus = resp.Status
		delay := retryDelay(attempt, resp.Header)
		// Drained and closed before sleeping: an unread body holds the
		// connection out of the pool, and the deadline with it.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBuffer))
		resp.Body.Close()

		fmt.Fprintf(os.Stderr, "  HTTP %d from %s %s — retrying in %s (%d of %d)\n",
			lastStatus, r.method, r.path, delay.Round(time.Second), attempt+1, r.retries)
		if attempt == 0 && !idempotentMethod(r.method) {
			fmt.Fprintf(os.Stderr, "  Note: %s is not idempotent — this may deliver the request more than once.\n", r.method)
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// sizedPath is one file a streamed body was built from, with the size (its share
// of Content-Length) and modification time measured at build time. A zero
// modTime means none was captured, and checkFilesUnchanged then claims nothing
// about it.
type sizedPath struct {
	path    string
	size    int64
	modTime time.Time
}

// pathBody is a body whose bytes come from files named on the command line.
// Only those can hit the mismatch explainBodySizeChange translates, and only
// those can be re-stat'ed by checkFilesUnchanged afterwards.
type pathBody interface {
	paths() []sizedPath
}

// explainUploadTimeout says which file was being sent when --timeout expired,
// or nil when the failure was not that. --timeout bounds the whole request,
// upload included, so a large file can hit it mid-send; a body with no file
// keeps its own message.
func explainUploadTimeout(err error, body api.BodySource, timeout time.Duration) error {
	src, ok := body.(pathBody)
	if !ok || !api.IsTimeout(err) {
		return nil
	}
	paths := src.paths()
	if len(paths) == 0 {
		return nil
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, p.path)
	}
	return fmt.Errorf("the request stopped at the CLI's own --timeout (%s) while %s was being sent — the instance may have received part of it; pass --timeout 0 (or a longer one) and send it again: %w",
		timeout, strings.Join(names, ", "), err)
}

// explainBodySizeChange names the file when a streamed body ran short of its
// measured length: a file that shrank between stat and send fails in net/http
// with "ContentLength=N with Body length M". A file that grew does not fail
// here; checkFilesUnchanged catches it.
func explainBodySizeChange(err error, body api.BodySource) error {
	src, ok := body.(pathBody)
	// ⚠️ Matched on net/http's error text, which has no sentinel; if the wording
	// changes the message silently reverts to the transport's, and
	// TestAPISaysWhenARealFileShrankWhileItWasSent is the guard.
	if !ok || !strings.Contains(err.Error(), "ContentLength=") {
		return err
	}
	paths := src.paths()
	if len(paths) == 0 {
		return err
	}
	// The transport cannot say which part of a multipart body ran short. Wrapped,
	// not replaced: api.Unanswered and api.IsTimeout inspect the chain.
	if len(paths) == 1 {
		return fmt.Errorf("%s changed size while it was being sent — send it again once it is complete: %w",
			paths[0].path, err)
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, p.path)
	}
	return fmt.Errorf("one of these changed size while it was being sent, and the transport cannot say which: %s — send the request again once they are all complete: %w",
		strings.Join(names, ", "), err)
}

// checkFilesUnchanged re-stats every file a streamed body was built from and
// reports one whose size or modification time changed since it was measured.
// ⚠️ A file that grew is not caught by net/http: it sends exactly Content-Length
// bytes, the server stores that prefix as a complete upload, and the transport's
// late error usually loses the race with the response; `ronja api` calls this
// after emitting the response.
func checkFilesUnchanged(body api.BodySource) error {
	src, ok := body.(pathBody)
	if !ok {
		return nil
	}
	var changed []string
	for _, p := range src.paths() {
		fi, err := os.Stat(p.path)
		if err != nil {
			// A vanished file is not evidence of a truncated upload: the send
			// read through a handle it already held.
			continue
		}
		if fi.Size() != p.size {
			changed = append(changed, fmt.Sprintf("%s changed size while it was being sent (%d → %d bytes) — the instance may have accepted the first %d bytes as a complete upload; send it again once the file is complete",
				p.path, p.size, fi.Size(), p.size))
			continue
		}
		// Same length, new modTime: rewritten in place mid-send, which neither
		// the transport nor the size check can see.
		if !p.modTime.IsZero() && !fi.ModTime().Equal(p.modTime) {
			changed = append(changed, fmt.Sprintf("%s was modified while it was being sent — it is still %d bytes, so the request went out at its full declared length, but what the instance received is part of the old file and part of the new one; send it again now that the file has settled",
				p.path, p.size))
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return errors.New(strings.Join(changed, "; "))
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func idempotentMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// retryDelay is the server's Retry-After (seconds or HTTP date) when it named
// one, else exponential backoff, capped at retryMaxDelay either way.
func retryDelay(attempt int, header http.Header) time.Duration {
	if header != nil {
		if after := strings.TrimSpace(header.Get("Retry-After")); after != "" {
			if secs, err := strconv.Atoi(after); err == nil && secs >= 0 {
				return capDelay(time.Duration(secs) * time.Second)
			}
			if when, err := http.ParseTime(after); err == nil {
				if d := time.Until(when); d > 0 {
					return capDelay(d)
				}
				// A date already in the past means "now" rather than "never".
				return retryBaseDelay
			}
		}
	}
	delay := retryBaseDelay << attempt
	if delay <= 0 { // overflow on an absurd --retry
		return retryMaxDelay
	}
	return capDelay(delay)
}

func capDelay(d time.Duration) time.Duration {
	if d > retryMaxDelay {
		return retryMaxDelay
	}
	return d
}

// sleepCtx waits interruptibly. Cancellation is checked before the select too:
// with a zero duration both cases are ready and select would pick at random.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type apiOutput struct {
	filter      *jqf.Filter
	raw         bool
	out         string
	failOnError bool
}

// buffered reports whether the whole body must be held in memory; --out alone
// streams.
func (o apiOutput) buffered() bool {
	return o.filter != nil || o.failOnError
}

// emit writes one response out, and reports whether the command failed. --out
// always gets the response verbatim, --jq writes the filtered view to stdout,
// and the unfiltered body reaches stdout only when neither claimed it.
func (o apiOutput) emit(ctx context.Context, resp *api.RawResponse, method, path string) error {
	if !o.buffered() {
		return o.emitStreaming(resp, method, path)
	}

	body, err := readCapped(resp.Body)
	if err != nil {
		return fmt.Errorf("read response from %s %s: %w", method, path, err)
	}
	if o.out != "" {
		if err := writeResultFile(o.out, string(body)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  %d bytes written to %s\n", len(body), o.out)
	}

	// An error body is still shown: it is the most useful thing the server said.
	if resp.Status >= 400 {
		if o.out == "" {
			os.Stdout.Write(body)
		}
		fmt.Fprintf(os.Stderr, "HTTP %d %s %s\n", resp.Status, method, path)
		return errAlreadyReported
	}

	// A failed DuckDB query, like any endpoint reporting failure in its
	// envelope, answers 200 with `error` set; without this the command exits 0.
	if o.failOnError {
		if msg := envelopeError(body); msg != "" {
			if o.out == "" {
				os.Stdout.Write(body)
			}
			return fmt.Errorf("%s %s returned HTTP %d with an error: %s", method, path, resp.Status, msg)
		}
	}

	if o.filter != nil {
		results, err := runFilter(ctx, o.filter, body)
		if err != nil {
			return err
		}
		return jqf.Render(os.Stdout, results, o.raw)
	}
	if o.out == "" {
		if _, err := os.Stdout.Write(body); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return nil
}

// runFilter applies a compiled filter to one response body under
// jqFilterTimeout. ⚠️ All or nothing: every matched value or an error, never a
// truncated prefix, because a jq stream has nowhere to say it was cut and a
// captured `$(… --jq …)` would silently hold a wrong prefix.
func runFilter(ctx context.Context, filter *jqf.Filter, body []byte) ([]any, error) {
	ctx, cancel := context.WithTimeout(ctx, jqFilterTimeout)
	defer cancel()

	return filter.RunBytes(ctx, body)
}

// emitStreaming copies the body without holding it: to the --out file, else to
// stdout.
func (o apiOutput) emitStreaming(resp *api.RawResponse, method, path string) error {
	if o.out != "" {
		n, err := writeStreamFile(o.out, resp.Body)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  %d bytes written to %s\n", n, o.out)
		if resp.Status >= 400 {
			fmt.Fprintf(os.Stderr, "HTTP %d %s %s\n", resp.Status, method, path)
			return errAlreadyReported
		}
		return nil
	}

	// Streamed before the status is judged, so an error body is still shown.
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return fmt.Errorf("read response from %s %s: %w", method, path, err)
	}
	if resp.Status >= 400 {
		fmt.Fprintf(os.Stderr, "HTTP %d %s %s\n", resp.Status, method, path)
		return errAlreadyReported
	}
	return nil
}

// readCapped reads a whole body, refusing anything past maxResponseBuffer.
func readCapped(r io.Reader) ([]byte, error) {
	// One byte past the cap tells "exactly at the limit" from "over it".
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBuffer+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBuffer {
		return nil, fmt.Errorf("the response is larger than the %d MiB this command can hold in memory — drop --jq/--fail-on-error to stream it, or use --out to save it",
			maxResponseBuffer>>20)
	}
	return body, nil
}

// envelopeError pulls a top-level `error` out of a JSON response, or "" when
// there is none. A body that is not a JSON object is never a failure, so a CSV
// or zip passes.
func envelopeError(body []byte) string {
	var shape struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return ""
	}
	return strings.TrimSpace(shape.Error)
}

type waitPlan struct {
	until    *jqf.Filter
	interval time.Duration
	timeout  time.Duration
}

var errWaitTimeout = errors.New("wait timed out")

// wait re-issues the request until the condition holds and returns the
// satisfying response, rebuilt from the bytes the condition was tested against
// so the caller's normal emit path handles it. Each polled body is capped like
// any buffered one.
func (r apiRequest) wait(ctx context.Context, plan waitPlan) (*api.RawResponse, error) {
	if plan.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, plan.timeout)
		defer cancel()
	}

	for attempt := 1; ; attempt++ {
		resp, err := r.send(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, waitFailure(plan, attempt, err)
			}
			return nil, err
		}
		body, readErr := readCapped(resp.Body)
		status := resp.Status
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read response from %s %s: %w", r.method, r.path, readErr)
		}

		// An HTTP error ends the wait: a 404 is a wrong path, not "not ready
		// yet". 429 and 5xx reach here only once --retry is spent or unset.
		if status >= 400 {
			os.Stdout.Write(body)
			fmt.Fprintf(os.Stderr, "HTTP %d %s %s (while waiting)\n", status, r.method, r.path)
			return nil, errAlreadyReported
		}

		results, err := runCondition(ctx, plan.until, body)
		if err != nil {
			// The wait's deadline expiring mid-evaluation is a timed-out wait,
			// not a broken filter.
			if ctx.Err() != nil {
				return nil, waitFailure(plan, attempt, ctx.Err())
			}
			return nil, err
		}
		if jqf.Holds(results) {
			// Not re-fetched: the printed answer must be the one that satisfied
			// the condition.
			return &api.RawResponse{
				Status: status,
				Header: resp.Header,
				Body:   io.NopCloser(strings.NewReader(string(body))),
			}, nil
		}

		if err := sleepCtx(ctx, plan.interval); err != nil {
			return nil, waitFailure(plan, attempt, err)
		}
	}
}

// runCondition evaluates a --wait-until condition against one response, all or
// nothing like runFilter: Holds needs every output truthy, so an all-truthy
// prefix would end the wait early. It gets jqFilterTimeout because
// --wait-timeout may be 0.
func runCondition(ctx context.Context, until *jqf.Filter, body []byte) ([]any, error) {
	ctx, cancel := context.WithTimeout(ctx, jqFilterTimeout)
	defer cancel()

	return until.RunBytes(ctx, body)
}

// waitFailure turns an expired wait into a message naming the condition.
func waitFailure(plan waitPlan, attempts int, cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s and %d %s: %s was never satisfied — raise --wait-timeout, or check the condition against one response first",
			errWaitTimeout, plan.timeout, attempts, plural(attempts, "attempt"), plan.until.Expr())
	}
	return cause
}
