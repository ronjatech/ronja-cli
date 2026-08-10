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

// What `ronja api` does with a response: send it, judge it, and emit it.
//
// The command started as "copy the body to stdout and check the status", which
// is the right default and is still the path taken when no flag asks for more.
// Everything here is the set of behaviours that turned out to be missing when
// people actually built things with it — filtering, waiting, saving, retrying —
// each of which needs the response held rather than streamed, and none of which
// knows a single thing about any endpoint.

// maxResponseBuffer bounds a response the command has to HOLD.
//
// Only the flags that must see the whole document pay this: --jq has to parse
// it, --wait-until has to test it, --fail-on-error has to look inside it. The
// default path and --out both still stream, so a large download is unaffected —
// which is why this can be a bound that fails loudly rather than a truncation
// that silently hands back a prefix.
const maxResponseBuffer = 64 << 20

// retryBaseDelay is the first pause before a retried attempt; it doubles up to
// retryMaxDelay. A 429 or a 5xx from a pod restarting mid-deploy clears in
// seconds, and starting lower just spends the budget before it does.
const (
	retryBaseDelay = 1 * time.Second
	retryMaxDelay  = 30 * time.Second
)

// DefaultWaitInterval is the cadence --wait-until polls at.
//
// 3s matches what the web app polls a running workflow at, and the interval is
// EXACT — it does not back off. A caller who names a cadence has said what they
// want, and a poller that quietly stretched to 30s would make a --wait-interval
// of 1s a lie. --wait-timeout is what bounds the total cost.
const (
	DefaultWaitInterval = 3 * time.Second
	DefaultWaitTimeout  = 5 * time.Minute
)

// apiRequest is one fully-resolved request, replayable.
//
// Replayable is the load-bearing word: --retry and --wait-until both issue it
// more than once, so the body is bytes rather than a reader. A reader is
// consumed by the first attempt and the second would send nothing at all — and
// an empty POST is the kind of thing a server accepts.
type apiRequest struct {
	client  *api.Client
	method  string
	path    string
	header  http.Header
	body    []byte
	timeout time.Duration
	retries int
}

// send issues the request once, retrying the answers that mean "ask again".
//
// Only 429 and 5xx are retried: they are the server saying it could not deal
// with the request, as opposed to a 4xx, which is it saying the request was
// wrong — and repeating a wrong request just spends the budget to be told so
// again. Transport failures are NOT retried either, because a connection that
// dropped mid-request may well have delivered it.
//
// The caller owns the returned body and must close it.
func (r apiRequest) send(ctx context.Context) (*api.RawResponse, error) {
	var lastStatus int
	for attempt := 0; ; attempt++ {
		resp, err := r.client.DoRaw(ctx, r.method, r.path, r.header, r.body, r.timeout)
		if err != nil {
			return nil, err
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

		// One line per retry, on stderr. A silent retry turns a 40-second
		// command into a mystery, and the status is the part that explains it.
		fmt.Fprintf(os.Stderr, "  HTTP %d from %s %s — retrying in %s (%d of %d)\n",
			lastStatus, r.method, r.path, delay.Round(time.Second), attempt+1, r.retries)
		// The one honest warning about what a retry means. Repeating a GET is
		// free; repeating a POST may commit it twice, and only the caller knows
		// whether that endpoint is idempotent.
		if attempt == 0 && !idempotentMethod(r.method) {
			fmt.Fprintf(os.Stderr, "  Note: %s is not idempotent — this may deliver the request more than once.\n", r.method)
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// retryableStatus reports the answers worth asking again.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// idempotentMethod reports the methods a repeat cannot change the meaning of.
func idempotentMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// retryDelay is the server's Retry-After when it named one, else exponential
// backoff.
//
// Honouring the header is the point of carrying response headers at all: a rate
// limiter that says "wait 30 seconds" and gets asked again in one has been told
// its instruction does not apply, which is how a soft limit becomes a hard one.
// Both header forms are accepted — a delay in seconds, and an HTTP date — since
// which one a proxy emits is not something a client gets to choose.
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

// sleepCtx waits, but stays interruptible. A Ctrl-C during a 30-second backoff
// has to be felt now, not after it.
//
// Cancellation is checked BEFORE the select, not only inside it. A zero or
// already-elapsed duration — a `Retry-After: 0`, a wait interval that has just
// expired — makes both cases ready at once, and select picks between ready
// cases at RANDOM. Without this check a cancelled context would be honoured
// only about half the time, which is the worst kind of bug: correct in most
// runs and not in the rest.
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

// apiOutput is what to do with the response once there is one.
type apiOutput struct {
	filter      *jqf.Filter
	raw         bool
	out         string
	failOnError bool
}

// buffered reports whether the whole body has to be held in memory.
//
// --out alone does not: it streams to the file. It is the flags that must read
// INTO the document — filter it, or look for an error inside it — that force the
// buffer, and saying so here keeps the streaming default from quietly eroding.
func (o apiOutput) buffered() bool {
	return o.filter != nil || o.failOnError
}

// emit writes one response out, and reports whether the command failed.
//
// Ordering is deliberate and matters when flags are combined: --out always gets
// the response VERBATIM (it is the archive of what the server said), --jq
// writes the filtered view to stdout, and the unfiltered body reaches stdout
// only when nothing else claimed it. So `--out resp.json --jq -r .id` saves the
// whole answer and prints the one field, which is what both flags plainly mean.
func (o apiOutput) emit(resp *api.RawResponse, method, path string) error {
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

	// The HTTP status is judged BEFORE the body is interpreted, and the body is
	// still shown: an error body is the most useful thing the server said, and a
	// runner that swallowed it would make every 400 a second round trip.
	if resp.Status >= 400 {
		if o.out == "" {
			os.Stdout.Write(body)
		}
		fmt.Fprintf(os.Stderr, "HTTP %d %s %s\n", resp.Status, method, path)
		return errAlreadyReported
	}

	// The 200-with-an-error trap, which `ronja query` has always handled and
	// this command could not: a failed DuckDB query — and any endpoint that
	// reports failure in its envelope — answers 200 with `error` set, so a
	// generic runner exits ZERO on something that never ran, and a pipeline
	// built on `&&` continues over a failure it never saw.
	if o.failOnError {
		if msg := envelopeError(body); msg != "" {
			if o.out == "" {
				os.Stdout.Write(body)
			}
			return fmt.Errorf("%s %s returned HTTP %d with an error: %s", method, path, resp.Status, msg)
		}
	}

	if o.filter != nil {
		results, err := o.filter.RunBytes(body)
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

// emitStreaming is the original path, unchanged in behaviour: the body is never
// held, only copied — to a file when --out asked for one, else to stdout.
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

	// Streamed, and BEFORE the status is judged, for the reason above.
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
	// One byte past the cap, so "exactly at the limit" and "over it" are
	// distinguishable rather than both looking full.
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
// there is none.
//
// Deliberately shallow, and deliberately not endpoint knowledge: this is the
// shape the whole API uses for a failure it reports in-band (the same field
// internal/api.parseError reads out of a non-2xx), so knowing it cannot drift
// the way a route table can. A body that is not JSON, or not an object, has no
// envelope and is therefore not a failure — this flag must never turn a
// perfectly good CSV or zip into an error.
func envelopeError(body []byte) string {
	var shape struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return ""
	}
	return strings.TrimSpace(shape.Error)
}

// waitPlan is the --wait-until loop's configuration.
type waitPlan struct {
	until    *jqf.Filter
	interval time.Duration
	timeout  time.Duration
}

// errWaitTimeout ends a wait that never came true. Its own error so the message
// can say what was being waited FOR, which is the only thing worth knowing.
var errWaitTimeout = errors.New("wait timed out")

// wait re-issues the request until the condition holds, and hands back the
// response that satisfied it.
//
// This is the shape the CLI's doctrine explicitly makes room for: a stateful
// multi-step loop that plain HTTP does badly. Ronja is full of asynchronous
// state machines — workflow runs, table builds, connector syncs, feature
// exports, managed-database provisioning — and every caller was hand-rolling
// the same sleep-and-poll around each of them. A verb per primitive would be a
// wrapper and would drift; this knows only how to re-send a request the caller
// already wrote, and works for every one of them, including the ones added
// after it.
//
// Each attempt's body must be buffered (the condition has to read it), so the
// polled response is capped like any other buffered one. The satisfying
// response is returned UNREAD, as a replayed final request, so the caller's
// normal emit path handles it identically to a one-shot call.
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

		// An HTTP error during a wait ENDS it. A 404 on the thing being polled
		// is not "not ready yet", it is the wrong path — and looping on it for
		// five minutes would report a typo as a timeout. (429 and 5xx never
		// reach here when --retry is set; without it they are a real answer.)
		if status >= 400 {
			os.Stdout.Write(body)
			fmt.Fprintf(os.Stderr, "HTTP %d %s %s (while waiting)\n", status, r.method, r.path)
			return nil, errAlreadyReported
		}

		results, err := plan.until.RunBytes(body)
		if err != nil {
			return nil, err
		}
		if jqf.Holds(results) {
			// Handed back as a response the caller can emit, so --jq, --out and
			// the plain body path all behave exactly as they would without a
			// wait. Nothing re-fetches: these are the bytes the condition was
			// tested against, which is also the only way the printed answer is
			// guaranteed to be the one that satisfied it.
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

// waitFailure turns a cancelled or expired wait into a message that says what
// was being waited for. "context deadline exceeded" after five silent minutes
// is the least useful thing this could report.
func waitFailure(plan waitPlan, attempts int, cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s and %d %s: %s was never satisfied — raise --wait-timeout, or check the condition against one response first",
			errWaitTimeout, plan.timeout, attempts, plural(attempts, "attempt"), plan.until.Expr())
	}
	return cause
}
