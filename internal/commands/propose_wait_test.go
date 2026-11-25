package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// fakeWaitedList is GET /api/v2/change-list/:id as the server answers it
// (backend api/v2/changelist/handler.go) for one list, changelist-1: each
// read takes the next answer, and the last one repeats.
type fakeWaitedList struct {
	server  *httptest.Server
	mu      sync.Mutex
	answers []fakeWaitAnswer
	queries []string
	// firstFailure is when the first non-2xx answer was served.
	firstFailure time.Time
}

type fakeWaitAnswer struct {
	status int
	body   string
	// date is the answer's Date header; zero leaves the server's own.
	date time.Time
}

func newFakeWaitedList(t *testing.T, answers ...fakeWaitAnswer) *fakeWaitedList {
	t.Helper()
	f := &fakeWaitedList{answers: answers}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	signInTo(t, f.server.URL)
	return f
}

func (f *fakeWaitedList) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/authentication/me":
		writeJSON(w, map[string]any{"user": map[string]any{"id": "user-ada", "first_name": "Ada", "last_name": "Lovelace"},
			"tenant": map[string]any{"id": testTenantID, "name": "Test"}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/change-list/"):
		if r.URL.Path != "/api/v2/change-list/changelist-1" {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "change list not found"})
			return
		}
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RawQuery)
		a := f.answers[0]
		if len(f.answers) > 1 {
			f.answers = f.answers[1:]
		}
		if a.status/100 != 2 && f.firstFailure.IsZero() {
			f.firstFailure = time.Now()
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !a.date.IsZero() {
			w.Header().Set("Date", a.date.UTC().Format(http.TimeFormat))
		}
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeWaitedList) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

// waitedList is a list's answer in a status, with fields set over it. Every
// answer carries a plan, as a Ronja that ignores ?omit=plan sends it.
func waitedList(status string, set map[string]any) fakeWaitAnswer {
	l := map[string]any{
		"id": "changelist-1", "status": status, "requestedFor": "user-ada", "reason": nil, "result": nil,
		"expiresAt": time.Now().Add(23 * time.Hour).UTC().Format(time.RFC3339), "decidedAt": nil, "finishedAt": nil,
		"plan": map[string]any{"dir": ".", "databases": []any{map[string]any{"dir": "db/crm", "databaseID": "mdb-1",
			"migrations": []any{map[string]any{"name": "0002_score", "sql": "ALTER TABLE plan_marker ADD score int"}}}}},
	}
	if status != "pending" {
		l["decidedAt"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	}
	for k, v := range set {
		l[k] = v
	}
	body, _ := json.Marshal(l)
	return fakeWaitAnswer{status: http.StatusOK, body: string(body)}
}

func failedAnswer(status int) fakeWaitAnswer {
	return fakeWaitAnswer{status: status, body: `{"error":"boom"}`}
}

// quickWait shortens the wait's clocks so a test runs in milliseconds.
func quickWait(t *testing.T) {
	t.Helper()
	saved := []time.Duration{proposeWaitFastPoll, proposeWaitFastWindow, proposeWaitSlowPoll, proposeWaitGrace, proposeWaitReadFailures}
	proposeWaitFastPoll, proposeWaitFastWindow, proposeWaitSlowPoll = 5*time.Millisecond, time.Minute, 5*time.Millisecond
	proposeWaitGrace, proposeWaitReadFailures = 5*time.Minute, 200*time.Millisecond
	t.Cleanup(func() {
		proposeWaitFastPoll, proposeWaitFastWindow, proposeWaitSlowPoll, proposeWaitGrace, proposeWaitReadFailures =
			saved[0], saved[1], saved[2], saved[3], saved[4]
	})
}

// runWait runs `ronja propose wait` and returns stdout, stderr and the error.
func runWait(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, t.TempDir(), append([]string{"propose", "wait"}, args...)...)
	})
	return out, stderr, err
}

func wantExit1(t *testing.T, err error, out string) {
	t.Helper()
	if err == nil || exitCodeFor(err) != 1 {
		t.Fatalf("exit %d (%v), want 1\nstdout:\n%s", exitCodeFor(err), err, out)
	}
}

func wantAll(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s does not say %q:\n%s", what, w, got)
		}
	}
}

var appliedResult = map[string]any{
	"verdict": "applied",
	"migrations": []any{map[string]any{"dir": "db/crm", "databaseID": "mdb-1", "report": map[string]any{
		"databaseID": "mdb-1", "dryRun": false, "notes": []any{},
		"results": []any{map[string]any{"name": "0001_leads", "outcome": "skipped"}, map[string]any{"name": "0002_score", "outcome": "applied"}}}}},
	"sync": map[string]any{"verdict": "applied", "folders": []any{
		map[string]any{"path": "workflows/report", "verdict": "applied", "units": []any{}},
		map[string]any{"path": "apps/dash", "verdict": "applied", "units": []any{}},
	}},
}

func TestProposeWaitApplied(t *testing.T) {
	quickWait(t)
	f := newFakeWaitedList(t, waitedList("pending", nil), waitedList("pending", nil), waitedList("applying", nil),
		waitedList("applied", map[string]any{"result": appliedResult, "finishedAt": time.Now().UTC().Format(time.RFC3339)}))

	out, stderr, err := runWait(t, "changelist-1")
	if err != nil {
		t.Fatalf("wait: %v\n%s", err, out)
	}
	wantAll(t, "the outcome", out, "deployed", "workflows/report", "apps/dash", "db/crm", "mdb-1", "1 migration applied")
	wantAll(t, "the progress", stderr, "Waiting for Ada Lovelace to approve change list changelist-1", "Approved; deploying")
	if n := strings.Count(stderr, "Waiting for"); n != 1 {
		t.Errorf("progress printed %d times for one status, want once:\n%s", n, stderr)
	}
	if strings.Contains(out, "deploying") || strings.Contains(stderr, "deployed") {
		t.Errorf("progress and outcome are not split between stderr and stdout:\nstdout:\n%s\nstderr:\n%s", out, stderr)
	}
	for _, q := range f.reads() {
		if q != "omit=plan" {
			t.Errorf("a read asked %q, want omit=plan", q)
		}
	}

	out, _, err = runWait(t, "changelist-1", "--json")
	if err != nil {
		t.Fatalf("wait --json: %v\n%s", err, out)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, out)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"decidedAt", "expiresAt", "finishedAt", "id", "result", "status"}; !slices.Equal(keys, want) {
		t.Errorf("--json keys %v, want %v (reason only when set, never the plan)", keys, want)
	}
	if strings.Contains(out, "plan_marker") || string(got["status"]) != `"applied"` {
		t.Errorf("--json carries the plan or the wrong status:\n%s", out)
	}
}

// The deploy job reports a folder a reviewer must approve as refused with
// reason submitted_for_review, which fails the list at stage sync; either way
// it is not live.
func TestProposeWaitAppliedNamesReview(t *testing.T) {
	quickWait(t)
	review := map[string]any{"path": "pipelines/sales", "verdict": "refused", "reason": "submitted_for_review", "detail": "an admin reviews it", "units": []any{}}
	applied := map[string]any{"verdict": "applied", "sync": map[string]any{"verdict": "applied", "folders": []any{
		map[string]any{"path": "workflows/report", "verdict": "applied"}, review}}}
	newFakeWaitedList(t, waitedList("applied", map[string]any{"result": applied}))
	out, _, err := runWait(t, "changelist-1")
	if err != nil {
		t.Fatalf("wait: %v\n%s", err, out)
	}
	wantAll(t, "the applied outcome", out, "workflows/report: deployed", "pipelines/sales: sent for review, not live")

	failed := map[string]any{"verdict": "failed", "stage": "sync", "reason": "1 folder did not finish", "sync": map[string]any{"verdict": "refused", "folders": []any{
		map[string]any{"path": "workflows/report", "verdict": "applied"}, review}}}
	newFakeWaitedList(t, waitedList("failed", map[string]any{"result": failed, "reason": "1 folder did not finish"}))
	out, _, err = runWait(t, "changelist-1")
	wantExit1(t, err, out)
	wantAll(t, "the failed outcome", out, "workflows/report: deployed", "pipelines/sales: sent for review, not live",
		"Every folder that did not go live was sent for review", "Tell the person; proposing again sends the same review.")
	if strings.Contains(out, "Fix what it names") {
		t.Errorf("a review-only failure says to fix and propose again, which files the same review again:\n%s", out)
	}
}

func TestProposeWaitRejected(t *testing.T) {
	quickWait(t)
	newFakeWaitedList(t, waitedList("pending", nil), waitedList("rejected", map[string]any{"reason": "not on a Friday"}))
	out, _, err := runWait(t, "changelist-1")
	wantExit1(t, err, out)
	wantAll(t, "the outcome", out, "rejected", "not on a Friday", "the person it was proposed for or an admin", "nothing was deployed")
}

func TestProposeWaitFailed(t *testing.T) {
	quickWait(t)
	for _, tc := range []struct {
		name   string
		reason string
		result any
		want   []string
	}{
		{"before the write", "the approved copy would now change more than was approved",
			map[string]any{"verdict": "failed", "stage": "before_write", "reason": "the approved copy would now change more than was approved",
				"refusals": []any{map[string]any{"folder": "workflows/report", "path": "main.py", "detail": "would now write code that was not approved"}}},
			[]string{"would now change more than was approved", "nothing was written", "workflows/report/main.py: would now write code that was not approved"}},
		{"in the migrations", "the migrations in db/crm did not apply, so nothing else was deployed",
			map[string]any{"verdict": "failed", "stage": "migrations", "reason": "the migrations in db/crm did not apply, so nothing else was deployed",
				"migrations": []any{map[string]any{"dir": "db/crm", "databaseID": "mdb-1", "error": "relation \"leads\" already exists"}}},
			[]string{"did not apply", "a migration failed, so no folder deployed", "db/crm", "relation \"leads\" already exists"}},
		{"in the sync", "1 folder did not finish",
			map[string]any{"verdict": "failed", "stage": "sync", "reason": "1 folder did not finish", "sync": map[string]any{"verdict": "refused", "folders": []any{
				map[string]any{"path": "workflows/report", "verdict": "applied"},
				map[string]any{"path": "apps/dash", "verdict": "refused", "reason": "not_deployed", "detail": "the build failed"}}}},
			[]string{"1 folder did not finish", "every migration applied; folders partly deployed", "workflows/report: deployed", "apps/dash: refused (not_deployed): the build failed"}},
		{"in the sync, a review beside a refusal", "2 folders did not finish",
			map[string]any{"verdict": "failed", "stage": "sync", "reason": "2 folders did not finish", "sync": map[string]any{"verdict": "refused", "folders": []any{
				map[string]any{"path": "pipelines/sales", "verdict": "refused", "reason": "submitted_for_review"},
				map[string]any{"path": "apps/dash", "verdict": "refused", "reason": "not_deployed", "detail": "the build failed"}}}},
			[]string{"pipelines/sales: sent for review, not live", "apps/dash: refused (not_deployed): the build failed", "Fix what it names"}},
		{"in the sync, with no folder unfinished", "the deploy did not finish: exit status 2",
			map[string]any{"verdict": "failed", "stage": "sync", "reason": "the deploy did not finish: exit status 2", "sync": map[string]any{"verdict": "refused", "folders": []any{
				map[string]any{"path": "workflows/report", "verdict": "applied"}}}},
			[]string{"workflows/report: deployed", "Fix what it names"}},
		{"with no result", "the deploy job could not be started", nil,
			[]string{"the deploy job could not be started", "The deploy never reported back; Ronja cannot say what landed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newFakeWaitedList(t, waitedList("failed", map[string]any{"reason": tc.reason, "result": tc.result}))
			out, _, err := runWait(t, "changelist-1")
			wantExit1(t, err, out)
			wantAll(t, "the outcome", out, append(tc.want, "propose again: what already deployed plans as unchanged")...)
		})
	}
}

func TestProposeWaitExpired(t *testing.T) {
	quickWait(t)
	newFakeWaitedList(t, waitedList("expired", map[string]any{"reason": "nobody approved it within a day"}))
	out, _, err := runWait(t, "changelist-1")
	wantExit1(t, err, out)
	wantAll(t, "the outcome", out, "expired", "nobody approved it within a day", "Nothing was deployed")
	if strings.Contains(out, "ronja propose wait") {
		t.Errorf("an expiry names a list to wait for:\n%s", out)
	}

	newFakeWaitedList(t, waitedList("expired", map[string]any{"reason": "replaced by the newer change list changelist-2"}))
	out, _, err = runWait(t, "changelist-1")
	wantExit1(t, err, out)
	wantAll(t, "the outcome", out, "replaced by the newer change list changelist-2", "run: ronja propose wait changelist-2")
}

func TestProposeWaitTimeout(t *testing.T) {
	quickWait(t)
	for status, want := range map[string]string{"pending": "still waiting for approval", "applying": "still deploying"} {
		t.Run(status, func(t *testing.T) {
			newFakeWaitedList(t, waitedList(status, nil))
			start := time.Now()
			out, _, err := runWait(t, "changelist-1", "--timeout", "100ms")
			wantExit1(t, err, out)
			wantAll(t, "the outcome", out, want, "ronja propose wait changelist-1")
			if waited := time.Since(start); waited < 100*time.Millisecond || waited > 5*time.Second {
				t.Errorf("--timeout 100ms ended after %s", waited)
			}
		})
	}
}

// A read in flight at --timeout is cut off there, not at the request's own
// 30 seconds: the list's read and the approver's read alike.
func TestProposeWaitTimeoutBoundsTheRead(t *testing.T) {
	quickWait(t)
	for name, hangMe := range map[string]bool{"the list's read": false, "the approver's read": true} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v2/authentication/me" && !hangMe:
					writeJSON(w, map[string]any{"user": map[string]any{"id": "user-ada", "first_name": "Ada", "last_name": "Lovelace"},
						"tenant": map[string]any{"id": testTenantID, "name": "Test"}})
					return
				case r.URL.Path == "/api/v2/change-list/changelist-1":
					mu.Lock()
					reads++
					first := reads == 1
					mu.Unlock()
					if first {
						a := waitedList("pending", nil)
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(a.body))
						return
					}
				}
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			signInTo(t, server.URL)
			start := time.Now()
			out, _, err := runWait(t, "changelist-1", "--timeout", "300ms")
			wantExit1(t, err, out)
			wantAll(t, "the outcome", out, "still waiting for approval")
			if took := time.Since(start); took > time.Second {
				t.Errorf("--timeout 300ms ended after %s", took)
			}
		})
	}
}

// Past the first window a pending list is read once a minute, while a
// deploying one is still read every 10 seconds.
func TestProposeWaitCadence(t *testing.T) {
	quickWait(t)
	proposeWaitFastWindow, proposeWaitSlowPoll = 0, time.Hour
	newFakeWaitedList(t, waitedList("applying", nil), waitedList("applying", nil),
		waitedList("applied", map[string]any{"result": appliedResult}))
	start := time.Now()
	if out, _, err := runWait(t, "changelist-1"); err != nil {
		t.Fatalf("wait: %v\n%s", err, out)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a deploying list past the first window was read slowly: %s", took)
	}

	f := newFakeWaitedList(t, waitedList("pending", nil))
	out, _, err := runWait(t, "changelist-1", "--timeout", "200ms")
	wantExit1(t, err, out)
	if n := len(f.reads()); n != 1 {
		t.Errorf("%d reads of a pending list past the first window in 200ms, want 1: the first, since a read due at --timeout is already cut off", n)
	}
}

// Ctrl-C and SIGTERM cancel the command's context: the wait ends at once and
// says the list is unaffected.
func TestProposeWaitStopsOnCancel(t *testing.T) {
	quickWait(t)
	proposeWaitFastPoll, proposeWaitSlowPoll = time.Hour, time.Hour
	f := newFakeWaitedList(t, waitedList("applying", nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	var err error
	captureStderr(t, func() { err = runProposeWait(ctx, api.New(f.server.URL, "token"), "changelist-1", 0) })
	if err == nil {
		t.Fatal("a cancelled wait ended with no error")
	}
	wantAll(t, "the error", err.Error(), "stopped waiting for change list changelist-1", "the list is unaffected", "ronja propose wait changelist-1")
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the wait took %s to stop", took)
	}
}

// Only the janitor closes a list that is past its expiry or stuck
// deploying; when it has not, the wait ends rather than waiting forever.
func TestProposeWaitNotClosed(t *testing.T) {
	quickWait(t)
	long := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for status, tc := range map[string]struct {
		set  map[string]any
		want string
	}{
		"pending":  {map[string]any{"expiresAt": long}, "it will not deploy"},
		"applying": {map[string]any{"decidedAt": long}, "cannot say what landed"},
	} {
		t.Run(status, func(t *testing.T) {
			newFakeWaitedList(t, waitedList(status, tc.set))
			out, _, err := runWait(t, "changelist-1")
			wantExit1(t, err, out)
			wantAll(t, "the outcome", out, "Ronja has not closed this list", tc.want)
		})
	}
}

// The two checks run on Ronja's clock, read from each answer's Date header: a
// machine whose clock runs ahead must not end a wait on a list Ronja holds open.
func TestProposeWaitNotClosedUsesServerClock(t *testing.T) {
	quickWait(t)
	long := time.Now().Add(-time.Hour)
	for status, set := range map[string]map[string]any{
		"pending":  {"expiresAt": long.UTC().Format(time.RFC3339)},
		"applying": {"decidedAt": long.UTC().Format(time.RFC3339)},
	} {
		t.Run(status, func(t *testing.T) {
			a := waitedList(status, set)
			a.date = long.Add(time.Minute)
			newFakeWaitedList(t, a)
			out, _, err := runWait(t, "changelist-1", "--timeout", "100ms")
			wantExit1(t, err, out)
			if strings.Contains(out, "has not closed") {
				t.Errorf("the wait ended on the local clock while Ronja's says the list is open:\n%s", out)
			}
			wantAll(t, "the outcome", out, "still")
		})
	}
}

// A connection that drops part-way through an answer is a read Ronja did not
// answer, tried again like any other.
func TestProposeWaitRetriesAnAnswerCutShort(t *testing.T) {
	quickWait(t)
	applied := waitedList("applied", map[string]any{"result": appliedResult})
	var mu sync.Mutex
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/change-list/changelist-1" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		reads++
		first := reads == 1
		mu.Unlock()
		if !first {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(applied.body))
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			len(applied.body), applied.body[:len(applied.body)/2])
		_ = buf.Flush()
	}))
	t.Cleanup(server.Close)
	signInTo(t, server.URL)
	out, _, err := runWait(t, "changelist-1")
	if err != nil {
		t.Fatalf("wait: %v\n%s", err, out)
	}
	wantAll(t, "the outcome", out, "deployed")
	mu.Lock()
	defer mu.Unlock()
	if reads != 2 {
		t.Errorf("%d reads, want the cut-short one and one more", reads)
	}
}

func TestProposeWaitReadErrors(t *testing.T) {
	quickWait(t)
	t.Run("not found ends at once", func(t *testing.T) {
		f := newFakeWaitedList(t, waitedList("pending", nil))
		out, _, err := runWait(t, "changelist-9")
		wantExit1(t, err, out)
		wantAll(t, "the error", err.Error(), "change list not found")
		if n := len(f.reads()); n != 0 {
			t.Errorf("%d reads of the other list after a 404", n)
		}
	})
	t.Run("forbidden ends at once", func(t *testing.T) {
		f := newFakeWaitedList(t, fakeWaitAnswer{status: http.StatusForbidden, body: `{"error":"change lists take a personal access token"}`})
		out, _, err := runWait(t, "changelist-1")
		wantExit1(t, err, out)
		wantAll(t, "the error", err.Error(), "change lists take a personal access token")
		if n := len(f.reads()); n != 1 {
			t.Errorf("%d reads after a 403, want 1", n)
		}
	})
	t.Run("a run of unanswered reads keeps waiting, then ends", func(t *testing.T) {
		f := newFakeWaitedList(t, waitedList("pending", nil), failedAnswer(503), failedAnswer(429), waitedList("pending", nil),
			failedAnswer(503), failedAnswer(502), failedAnswer(503))
		out, _, err := runWait(t, "changelist-1")
		wantExit1(t, err, out)
		ended := time.Now()
		wantAll(t, "the error", err.Error(), "503", "the list is unaffected", "run `ronja propose wait changelist-1` again")
		if n := len(f.reads()); n < 6 {
			t.Errorf("%d reads, want the wait to keep reading through the failures", n)
		}
		f.mu.Lock()
		first := f.firstFailure
		f.mu.Unlock()
		if since := ended.Sub(first); since < proposeWaitReadFailures {
			t.Errorf("ended %s after the first failure, want at least %s", since, proposeWaitReadFailures)
		}
	})
	t.Run("an answer that does not decode ends at once", func(t *testing.T) {
		f := newFakeWaitedList(t, fakeWaitAnswer{status: http.StatusOK, body: "<html>sign in</html>"})
		out, _, err := runWait(t, "changelist-1")
		wantExit1(t, err, out)
		wantAll(t, "the error", err.Error(), "ronja api /api/v2/change-list/changelist-1")
		if n := len(f.reads()); n != 1 {
			t.Errorf("%d reads after an undecodable answer, want 1", n)
		}
	})
	t.Run("no id refuses and names ./wait", func(t *testing.T) {
		newFakeWaitedList(t, waitedList("pending", nil))
		out, _, err := runWait(t)
		wantExit1(t, err, out)
		wantAll(t, "the refusal", err.Error(), "ronja propose ./wait")
	})
}

// propose still takes a directory beside its subcommands, and each bare
// subcommand names the ./ form that proposes a folder of its name.
func TestProposeStillTakesADirectory(t *testing.T) {
	newFakeWaitedList(t, waitedList("pending", nil))
	_, err := runCLI(t, t.TempDir(), "propose", "--no-browser", "no-such-dir")
	if err == nil || strings.Contains(err.Error(), "unknown command") || !strings.Contains(err.Error(), "no-such-dir") {
		t.Fatalf("propose no-such-dir: %v, want propose's own refusal naming the directory", err)
	}
	for _, verb := range []string{"wait", "describe", "send", "discard"} {
		_, err := runCLI(t, t.TempDir(), "propose", verb)
		if err == nil || !strings.Contains(err.Error(), "run: ronja propose ./"+verb) {
			t.Errorf("propose %s with no id: %v, want it to name ronja propose ./%s", verb, err, verb)
		}
		_, err = runCLI(t, t.TempDir(), "propose", "--no-browser", "./"+verb)
		if err == nil || strings.Contains(err.Error(), "change list's id") || !strings.Contains(err.Error(), verb) {
			t.Errorf("propose ./%s: %v, want propose's own refusal naming the directory", verb, err)
		}
	}
}

// The negative control: propose prints its next step and returns at once,
// never reading the list it just uploaded. An older Ronja's list is sent at
// once, so its next step is the wait.
func TestProposePrintsWaitAndDoesNotWait(t *testing.T) {
	f, dir := proposeFixture(t)
	start := time.Now()
	out, err := runPropose_(t, dir, "--stack", "dev")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("propose took %s", took)
	}
	wantAll(t, "propose", out, "ronja propose describe changelist-1 .change.json")
	f.changeLists.olderServer = true
	if out, err = runPropose_(t, dir, "--stack", "dev"); err != nil {
		t.Fatalf("propose to an older Ronja: %v\n%s", err, out)
	}
	wantAll(t, "propose to an older Ronja", out, "Nothing changes until", "To continue once it is decided, run: ronja propose wait changelist-1")
	f.mu.Lock()
	requests := slices.Clone(f.requests)
	f.mu.Unlock()
	for _, r := range requests {
		if strings.HasPrefix(r, "GET /api/v2/change-list") {
			t.Errorf("propose read the list after the upload: %s", r)
		}
	}

	unchanged, restore := captureStdout(t)
	printPropose(&proposeReport{Status: proposeStatusUnchanged})
	if got := restore(unchanged); strings.Contains(got, "propose wait") {
		t.Errorf("Nothing to propose prints the wait:\n%s", got)
	}
}
