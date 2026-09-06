package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Unanswered decides whether a CLI may say "the instance did not answer" and
// carry on. Its membership is therefore a safety property, not a convenience:
// every error it accepts is one some caller will WARN about and then keep
// writing files.
//
// The cases are built through a real client against a real server wherever the
// wrapping matters, because what Unanswered actually sees is `%w`-nested — a
// *url.Error inside a "GET …" wrapper — and a hand-built sentinel would test a
// shape the code never receives.
func TestUnansweredMembership(t *testing.T) {
	answers := func(status int) *Client {
		return serve(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"nope"}`, status)
		})
	}
	// A server that is gone: the connection error every genuine outage arrives
	// as. Closed before the request rather than during it, so nothing races.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	get := func(c *Client, ctx context.Context) error {
		var out map[string]any
		return c.Do(ctx, http.MethodGet, "feature/feat-1", nil, &out)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		err  func() error
		want bool
	}{
		// The instance never delivered a verdict.
		{"429", func() error { return get(answers(http.StatusTooManyRequests), context.Background()) }, true},
		{"500", func() error { return get(answers(http.StatusInternalServerError), context.Background()) }, true},
		{"503", func() error { return get(answers(http.StatusServiceUnavailable), context.Background()) }, true},
		{"connection refused", func() error { return get(New(deadURL, "t"), context.Background()) }, true},

		// The reader stopped us. This is the one that matters most: it arrives
		// as a *url.Error exactly like an outage does, and reading it as one
		// turns Ctrl-C into permission to scaffold a folder.
		{"context canceled", func() error { return get(answers(http.StatusOK), cancelled) }, false},
		{"bare context.Canceled", func() error { return context.Canceled }, false},
		{"bare context.DeadlineExceeded", func() error { return context.DeadlineExceeded }, false},

		// The OTHER deadline shape. http.Client.Timeout never produces
		// context.DeadlineExceeded — it produces a *url.Error that merely
		// reports Timeout() — so a membership test written against the sentinel
		// alone read this one as an outage while reading its per-request twin as
		// a deadline. Same event, two verdicts.
		{"client timeout", func() error { return clientTimeoutError() }, false},

		// An answer arrived, or we never got as far as asking.
		{"400", func() error { return get(answers(http.StatusBadRequest), context.Background()) }, false},
		{"404", func() error { return get(answers(http.StatusNotFound), context.Background()) }, false},
		{"decode response", func() error {
			c := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) })
			return get(c, context.Background())
		}, false},
		{"build request", func() error {
			return answers(http.StatusOK).Do(context.Background(), "bad method", "feature/feat-1", nil, nil)
		}, false},
		{"nil", func() error { return nil }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err()
			if tc.name != "nil" && err == nil {
				t.Fatal("the case produced no error at all, so it tests nothing")
			}
			if got := Unanswered(err); got != tc.want {
				t.Errorf("Unanswered(%v) = %v, want %v", err, got, tc.want)
			}
			if tc.name == "context canceled" {
				// The case is only worth having if it really does arrive in the
				// shape an outage does. A cancellation that stopped being a
				// *url.Error would pass this test while testing nothing.
				var urlErr *url.Error
				if !errors.As(err, &urlErr) {
					t.Errorf("a cancelled request no longer arrives as a *url.Error (%T), so this case no longer guards the arm it was written for", err)
				}
			}
		})
	}
}

// clientTimeoutError builds the error http.Client.Timeout produces: a
// *url.Error whose wrapped error says nothing but Timeout(). It is built by
// hand because the real thing takes a real expiring client, and what matters
// here is the SHAPE — a url.Error carrying no context sentinel at all.
func clientTimeoutError() error {
	return &url.Error{
		Op:  "Get",
		URL: "http://instance.invalid/api/v2/feature/feat-1",
		Err: timeoutErr{},
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "net/http: timeout awaiting response headers" }
func (timeoutErr) Timeout() bool { return true }

// The client-timeout shape has to answer both questions the way the per-request
// deadline does: not an outage, and a write that may well have landed.
func TestClientTimeoutIsATimeoutAndNotAnOutage(t *testing.T) {
	err := clientTimeoutError()
	if !IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false — the client's own Timeout is a deadline like any other", err)
	}
	if Unanswered(err) {
		t.Errorf("Unanswered(%v) = true — a deadline stopped the request at our end, not the instance's", err)
	}
}

// A cancelled request is not a timeout either, and the two questions are
// deliberately separate: IsTimeout answers "the write may have landed, go and
// look", which is not what Ctrl-C before a read means.
func TestIsTimeoutIgnoresACancelledContext(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out map[string]any
	err := c.Do(ctx, http.MethodGet, "feature/feat-1", nil, &out)
	if err == nil {
		t.Fatal("a cancelled request came back clean")
	}
	if IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = true for a cancelled context", err)
	}
}

// AsNameTaken has to be gated on the CODE and not on the 409, because three
// unrelated refusals wear that status on this client: an optimistic-concurrency
// conflict on a draft commit, a file precondition, and this. Reading a name
// collision as one of the first two sends the author to `pipeline discard` or
// `--overwrite-remote` — neither of which frees a name, and one of which throws
// away a colleague's version to no purpose.
func TestAsNameTakenReadsTheCodeAndReturnsTheServersSentence(t *testing.T) {
	const sentence = `a table named "orders" already exists in this feature (table-d1abc)`
	err := errorFrom(409, []byte(`{"error":`+strconv.Quote(sentence)+`,"code":"name_taken"}`))

	why, taken := AsNameTaken(err)
	if !taken {
		t.Fatalf("AsNameTaken(%v) = false for the refusal it exists to recognise", err)
	}
	// The message NAMES the row in the way. Paraphrasing it would leave the
	// author with a rule and nothing to act on.
	if why != sentence {
		t.Errorf("message = %q, want the server's own sentence %q", why, sentence)
	}
}

func TestAsNameTakenIgnoresEveryOther409AndNonHTTPError(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"a bare 409", errorFrom(409, []byte(`{"error":"the table moved since your draft forked"}`))},
		{"a 409 carrying a different code", errorFrom(409, []byte(`{"error":"busy","code":"run_in_flight"}`))},
		{"a body that is not JSON", errorFrom(409, []byte("conflict"))},
		{"not an HTTP error at all", errors.New("name_taken")},
		{"no error", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if why, taken := AsNameTaken(tc.err); taken {
				t.Errorf("AsNameTaken = true (%q) — the code is the only discriminator", why)
			}
		})
	}
}

// A refusal it has CLAIMED must never come back with an empty sentence: the
// caller prints it, and a blank line under "that name is taken" is worse than
// the raw error.
func TestAsNameTakenNeverReturnsAnEmptySentence(t *testing.T) {
	why, taken := AsNameTaken(errorFrom(409, []byte(`{"code":"name_taken"}`)))
	if !taken {
		t.Fatal("AsNameTaken = false for a body carrying the code")
	}
	if strings.TrimSpace(why) == "" {
		t.Error("message is empty — it must fall back to the error's own rendering")
	}
}
