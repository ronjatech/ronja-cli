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

// jqFilterTimeout bounds ONE application of a jq filter.
//
// ⚠️ THE COMMAND'S OWN CONTEXT IS NOT A BOUND. cmd.Context() carries no
// deadline, and --timeout bounds the REQUEST, not what is done with the answer
// — so `ronja query --jq 'def f: f; f'` had nothing at all standing between it
// and a wedged terminal, and --wait-timeout 0 left the poll condition equally
// unbounded. jqf's step budget refuses a runaway program on its own, but not
// the shapes a step count cannot see (one enormous value costs a handful of
// instructions), which is what this is for.
//
// Ten seconds is enormous for the work: the body is already in memory and the
// heaviest realistic filter over one costs single-digit milliseconds. It is the
// third of the three bounds a filter runs under — the other two are jqf's work
// and memory budgets — and like them it FAILS rather than truncating, because
// half an answer presented as a whole one is the outcome none of them may have.
const jqFilterTimeout = 10 * time.Second

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
// more than once, so the body is a BodySource rather than a reader. A reader is
// consumed by the first attempt and the second would send nothing at all — and
// an empty POST is the kind of thing a server accepts. Each attempt below calls
// DoRaw again, which Opens the source afresh and closes what it opened.
type apiRequest struct {
	client  *api.Client
	method  string
	path    string
	header  http.Header
	body    api.BodySource
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

// sizedPath is one file a streamed body was built from, with the size that was
// measured for it at build time — the number that became its share of the
// request's Content-Length — and the modification time it carried when that
// measurement was taken.
//
// The modTime is what makes the after-the-fact check able to see a rewrite that
// kept the length. It is optional: a zero value means the build-time modTime
// was never captured, and checkFilesUnchanged then says nothing about it rather
// than treating "unknown" as "changed".
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
// or nil when the failure was not that.
//
// --timeout bounds ONE REQUEST, and a request includes the body going up. That
// is invisible until the body is large: a multi-gigabyte -F file=@big.bin dies
// partway through the send, with an error about a deadline and nothing about
// the upload — and the caller's reasonable reading, that the instance is slow,
// is wrong. The remedy is also not the obvious one, because the default (120s)
// is generous for every request that is not an upload and lowering the file
// size is not an option, so the message names --timeout 0 explicitly.
//
// Narrowed by the body's own type, like explainBodySizeChange: a request with
// no file in it times out for its own reasons and must keep its own message.
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
	// The deadline is worth quoting because it is the thing to change, and a
	// caller who did not pass --timeout has no idea what the default is.
	return fmt.Errorf("the request stopped at the CLI's own --timeout (%s) while %s was being sent — the instance may have received part of it; pass --timeout 0 (or a longer one) and send it again: %w",
		timeout, strings.Join(names, ", "), err)
}

// explainBodySizeChange names the file when a streamed body ran SHORT of the
// length that was measured for it.
//
// A file body is stat'ed once and sent later, so the file can change in
// between, and the two directions fail differently. A file that SHRANK ends
// before the Content-Length already written, and net/http fails the request
// outright with "http: ContentLength=N with Body length M" — which is the
// honest outcome, and this is where it is translated. What the raw error does
// not say is WHICH file, or that there is nothing wrong with the command. A
// retry does not re-stat, so a file still being written produces this same
// message on every attempt rather than a different one each time.
//
// A file that GREW does NOT fail here — see checkFilesUnchanged, which is the
// other half of this window.
func explainBodySizeChange(err error, body api.BodySource) error {
	src, ok := body.(pathBody)
	// Matched on the transport error's text because net/http offers no
	// sentinel for it: the error is built with errors.New at the point of the
	// write. Narrowed by the body's own type, so nothing else can be caught by
	// it — a request with no file in it never reaches this line.
	//
	// ⚠️ The substring is a dependency on net/http's own wording, verified
	// against Go 1.26.5 ("http: ContentLength=%d with Body length %d" in
	// transfer.go). Nothing here fails loudly if that text changes — the
	// message simply reverts to the transport's — so the guard is
	// TestAPISaysWhenARealFileShrankWhileItWasSent, which drives a real short
	// file through a real transport and asserts the translated message. If that
	// test fails after a Go upgrade, this is the line to look at.
	if !ok || !strings.Contains(err.Error(), "ContentLength=") {
		return err
	}
	paths := src.paths()
	if len(paths) == 0 {
		return err
	}
	// The transport error says a body was short; it does not say which part of
	// a multipart body it came from. With one file the message can be definite,
	// with several it must not pretend to be.
	// Wrapped rather than replaced: the transport error is the evidence for
	// everything this message asserts, and a caller that reaches for errors.Is
	// or errors.As on it (api.Unanswered and api.IsTimeout both do) must not
	// find a bare string where the chain used to be.
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
// reports one that is no longer the file that was measured for it — a different
// size, or the same size with a different modification time.
//
// This is the half of the stat-then-send window the transport does NOT catch,
// and the asymmetry is not obvious. A file that shrank fails in net/http, as
// explainBodySizeChange describes. A file that GREW does not: net/http writes
// exactly Content-Length bytes off a LimitReader, so the request on the wire is
// complete and valid — the server reads a whole upload of the declared size and
// answers 200 — and the "ContentLength=N with Body length M" error is raised
// only afterwards, racing the response in persistConn.roundTrip. Measured, it
// is usually LOST: the command exits zero and the instance has stored a prefix
// of the file as a finished upload.
//
// So growth is caught here instead: one re-stat, after the response has been
// emitted (see the call site in `ronja api` — the caller sees what the server
// said, and THEN a non-zero exit explaining what those bytes actually were).
//
// The re-stat compares the modification time as well as the size, because a
// file rewritten IN PLACE to the SAME length is invisible to both halves
// otherwise: nothing for the transport to fail on, and a size that matches. It
// is the quietest member of this family — a mixed-content upload that exits
// zero — so it is worth the second field.
func checkFilesUnchanged(body api.BodySource) error {
	src, ok := body.(pathBody)
	if !ok {
		return nil
	}
	var changed []string
	for _, p := range src.paths() {
		fi, err := os.Stat(p.path)
		if err != nil {
			// Nothing left to compare against. A file that vanished after its
			// bytes were read is not evidence of a truncated upload — the send
			// read through a handle it already held — and manufacturing a
			// failure out of a missing stat would fail correct commands.
			continue
		}
		if fi.Size() != p.size {
			changed = append(changed, fmt.Sprintf("%s changed size while it was being sent (%d → %d bytes) — the instance may have accepted the first %d bytes as a complete upload; send it again once the file is complete",
				p.path, p.size, fi.Size(), p.size))
			continue
		}
		// Same length, different modification time: the file was rewritten IN
		// PLACE while it was being read. Size alone cannot see this, and it is
		// the one case that passes both halves of the window — the transport
		// wrote exactly Content-Length bytes, so there is no mismatch for
		// net/http to fail on, and the re-stat above finds the number it
		// expected. What reached the instance is part of the old file and part
		// of the new one, accepted as a complete upload.
		//
		// A zero build-time modTime means nothing was captured to compare
		// against, so nothing is claimed: "not known" is not "changed".
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
// only when nothing else claimed it. So `--out resp.json --jq .id -r` saves the
// whole answer and prints the one field, which is what both flags plainly mean.
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

// runFilter applies a compiled filter to one response body, under a deadline.
//
// THE RULE IT ENFORCES IS ALL-OR-NOTHING: every value the filter matched is
// returned, or an error is, and there is no third outcome. Nothing is rendered
// until the whole stream has been collected, so a filter that fails at value
// 15,000 prints nothing rather than 14,999 lines and then a failure.
//
// ⚠️ AN EARLIER FORM TRUNCATED AT 10,000 VALUES with a note on stderr and a
// zero exit, and that is why the rule is written down here rather than assumed.
// `ids=$(ronja api ... --jq '.result[].id' -r 2>/dev/null)` throws the note away
// and captures a silently wrong PREFIX — and unlike `ronja query`'s
// `truncated:true`, which is IN the data, a jq stream has nowhere to put such a
// field. A --jq-strict flag was tried and only moved the problem: the default
// still lost data, and turning it on printed nothing at all for an answer that
// had been printable in full. What bounds an enormous answer now is jqf's memory
// budget, which fails loudly and names a remedy.
//
// The deadline is separate from --timeout (which bounds the REQUEST) because
// cmd.Context() carries none of its own: `--jq 'def f: f; f'` had nothing at all
// standing between it and a wedged terminal.
func runFilter(ctx context.Context, filter *jqf.Filter, body []byte) ([]any, error) {
	ctx, cancel := context.WithTimeout(ctx, jqFilterTimeout)
	defer cancel()

	return filter.RunBytes(ctx, body)
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

		results, err := runCondition(ctx, plan.until, body)
		if err != nil {
			// The WAIT's deadline can expire while the condition is being
			// evaluated (a slow box, a --wait-timeout of a few ms). That is a
			// timed-out wait, not a broken filter, and must read as one —
			// jqf's own message quotes the expression with %q, so the
			// condition the reader is looking for is not even in it verbatim.
			if ctx.Err() != nil {
				return nil, waitFailure(plan, attempt, ctx.Err())
			}
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

// runCondition evaluates a --wait-until condition against one response.
//
// It is the same all-or-nothing contract as runFilter, and a condition is where
// that matters most: Holds requires EVERY output to be truthy, so a PREFIX that
// happened to be all-truthy would end the poll on a response whose next value
// was false — a wait reporting success for work that never finished. Nothing
// truncates any more, so that cannot happen; when this path did truncate it had
// to refuse outright, which is the asymmetry that is now gone.
//
// The condition gets the same deadline as any other filter: --wait-timeout may
// legitimately be 0, so it cannot be relied on to bound one evaluation.
func runCondition(ctx context.Context, until *jqf.Filter, body []byte) ([]any, error) {
	ctx, cancel := context.WithTimeout(ctx, jqFilterTimeout)
	defer cancel()

	return until.RunBytes(ctx, body)
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
