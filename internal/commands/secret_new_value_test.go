package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The new-value fake answers a script of GET /secret/:id reads (the last one
// repeats); any other request fails the test, above all a PUT, which only the
// person's browser may send. Every answer carries the sentinel as a stray
// field, so a CLI that printed what it read would leak it.

const (
	newValueID  = "secret-key-1"
	newValueURL = "https://app.ronja.test/secrets/secret-key-1?newValue=1&org=tenant-abc"
)

var (
	changedT0 = "2026-11-01T10:00:00Z"
	changedT1 = "2026-11-07T12:00:00Z"
)

type newValueFake struct {
	t      *testing.T
	mu     sync.Mutex
	reads  int
	script []map[string]any
	// onRead, when set, runs before the n-th read (1-based) is answered.
	onRead func(n int)
}

func newNewValueFake(t *testing.T, script ...map[string]any) *newValueFake {
	t.Helper()
	f := &newValueFake{t: t, script: script}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	signInTo(t, server.URL)
	return f
}

func (f *newValueFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet || r.URL.Path != "/api/v2/secret/"+newValueID {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if len(f.script) == 0 {
		f.t.Errorf("GET /secret/:id with no scripted answer")
		http.Error(w, "no answer", http.StatusInternalServerError)
		return
	}
	a := f.script[0]
	if len(f.script) > 1 {
		f.script = f.script[1:]
	}
	f.reads++
	if f.onRead != nil {
		f.onRead(f.reads)
	}
	if code, ok := a["httpStatus"].(int); ok {
		writeJSONStatus(w, code, map[string]any{"error": a["error"]})
		return
	}
	body := map[string]any{
		"id": newValueID, "name": "Stripe", "status": "ready", "secretType": "api_key",
		"credentials": map[string]any{"apiKey": secretSentinel},
	}
	for k, v := range a {
		body[k] = v
	}
	writeJSON(w, body)
}

// nvAnswer is one detail read, its verdict from a probe unless kv says
// otherwise; nil values are left out of the JSON.
func nvAnswer(kv ...any) map[string]any {
	a := map[string]any{"newValue": map[string]any{"url": newValueURL}, "lastCheckSource": "probe"}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(a, kv[i].(string))
			continue
		}
		a[kv[i].(string)] = kv[i+1]
	}
	return a
}

func fastNewValue(t *testing.T, limit, checkWait time.Duration) {
	t.Helper()
	fastInBrowser(t, limit)
	saved := newValueCheckWait
	newValueCheckWait = checkWait
	t.Cleanup(func() { newValueCheckWait = saved })
}

func runNewValue(t *testing.T, args ...string) (stdout, stderr, errText string) {
	t.Helper()
	stdout, stderr, errText = runSecretCLI(t, append([]string{"secret", "new-value"}, args...)...)
	assertNoSentinel(t, stdout, stderr, errText)
	return stdout, stderr, errText
}

const notCheckedLine = "Saved. Ronja hasn't checked the new value yet; the health section shows it when she has.\n"

func TestSecretNewValueRefusedBeforeAnything(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first map[string]any
		want  string
	}{
		{"the server's refusal", nvAnswer("newValue", map[string]any{"refusal": "Only Anna Berg (its owner) or an Admin can change this secret."}), "Only Anna Berg (its owner) or an Admin can change this secret."},
		{"an older server", nvAnswer("newValue", nil), newValueOlderServer},
		{"not found", map[string]any{"httpStatus": 404, "error": "not found"}, "secret " + newValueID + " was not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastNewValue(t, 5*time.Second, time.Second)
			opened := fakeBrowser(t)
			f := newNewValueFake(t, tc.first)
			stdout, _, errText := runNewValue(t, newValueID)
			if !strings.Contains(errText, tc.want) {
				t.Fatalf("error = %q, want it to contain %q", errText, tc.want)
			}
			if stdout != "" || len(*opened) != 0 || f.reads != 1 {
				t.Fatalf("a refusal printed %q, opened %v, read %d times", stdout, *opened, f.reads)
			}
		})
	}
}

func TestSecretNewValueResultLines(t *testing.T) {
	// Only a refusal is a failure: the old value is gone and the stored one is
	// known not to work. A check not made, or one that couldn't reach the
	// service, is an unknown.
	refusedErr := "the new value for secret " + newValueID + " is saved, but the service refused it; the old value is gone, so enter a working one with `ronja secret new-value " + newValueID + "`"
	for _, tc := range []struct {
		name, status, errMsg, want, wantErr string
	}{
		{"works", "ok", "", "Saved. Ronja tried the new value and it works.\n", ""},
		{"refused", "auth_failed", "401 Unauthorized: invalid API key", "Saved, but the service refused the new value: 401 Unauthorized: invalid API key\n", refusedErr},
		{"refused without a reason", "auth_failed", "", "Saved, but the service refused the new value.\n", refusedErr},
		{"couldn't check", "error", "the host did not answer within 8s", "Saved. Ronja couldn't check it: the host did not answer within 8s\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastNewValue(t, 5*time.Second, 5*time.Second)
			opened := fakeBrowser(t)
			newNewValueFake(t,
				nvAnswer("lastCheckStatus", "ok", "lastCheckedAt", changedT0),
				nvAnswer("lastCheckStatus", "ok", "lastCheckedAt", changedT0),
				nvAnswer("valueChangedAt", changedT1, "lastCheckedAt", changedT1),
				nvAnswer("valueChangedAt", changedT1, "lastCheckStatus", tc.status, "lastCheckError", tc.errMsg, "lastCheckedAt", "2026-11-07T12:00:02Z"),
			)
			stdout, stderr, errText := runNewValue(t, newValueID)
			if errText != tc.wantErr {
				t.Fatalf("error = %q, want %q\nstderr: %s", errText, tc.wantErr, stderr)
			}
			if stdout != tc.want {
				t.Fatalf("stdout = %q, want %q", stdout, tc.want)
			}
			if len(*opened) != 1 || (*opened)[0] != newValueURL {
				t.Fatalf("browser handed %v, want exactly the server's link", *opened)
			}
			for _, want := range []string{"The value goes straight to Ronja; this terminal never sees it.", "Ctrl-C to stop waiting"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr lacks %q:\n%s", want, stderr)
				}
			}
		})
	}
}

// A token refresh or a login moves updatedAt; only valueChangedAt is a new value.
func TestSecretNewValueIgnoresUpdatedAt(t *testing.T) {
	fastNewValue(t, 50*time.Millisecond, time.Second)
	fakeBrowser(t)
	newNewValueFake(t,
		nvAnswer("valueChangedAt", changedT0, "updatedAt", changedT0, "lastCheckStatus", "ok", "lastCheckedAt", changedT0),
		nvAnswer("valueChangedAt", changedT0, "updatedAt", changedT1, "lastCheckStatus", "ok", "lastCheckedAt", changedT1),
	)
	stdout, _, errText := runNewValue(t, newValueID)
	if !strings.Contains(errText, "still waiting for a new value") || !strings.Contains(errText, "keeps its current value") {
		t.Fatalf("error = %q, want the wait to run out", errText)
	}
	if stdout != "" {
		t.Fatalf("an updatedAt change was reported as a saved value: %q", stdout)
	}
}

// The verdict a secret carries when its value changes describes the old value.
func TestSecretNewValueNeverReportsTheOldVerdict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first map[string]any
	}{
		{"never replaced before", nvAnswer("lastCheckStatus", "ok", "lastCheckedAt", changedT0)},
		{"replaced before", nvAnswer("valueChangedAt", changedT0, "lastCheckStatus", "ok", "lastCheckedAt", changedT0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastNewValue(t, 5*time.Second, 20*time.Millisecond)
			fakeBrowser(t)
			newNewValueFake(t, tc.first,
				nvAnswer("valueChangedAt", changedT1, "lastCheckStatus", "ok", "lastCheckedAt", "2026-11-07T11:59:59Z"))
			stdout, stderr, errText := runNewValue(t, newValueID)
			if errText != "" {
				t.Fatalf("failed: %s\nstderr: %s", errText, stderr)
			}
			if stdout != notCheckedLine {
				t.Fatalf("stdout = %q, want %q", stdout, notCheckedLine)
			}
		})
	}
}

func TestSecretNewValueNoBrowserPrintsTheLink(t *testing.T) {
	fastNewValue(t, 5*time.Second, 20*time.Millisecond)
	opened := fakeBrowser(t)
	newNewValueFake(t, nvAnswer(), nvAnswer("valueChangedAt", changedT1))
	stdout, stderr, errText := runNewValue(t, newValueID, "--no-browser")
	if errText != "" {
		t.Fatalf("failed: %s", errText)
	}
	if len(*opened) != 0 {
		t.Fatalf("a browser opened with --no-browser: %v", *opened)
	}
	if !strings.Contains(stderr, "Open this link to enter the new value in Ronja") || !strings.Contains(stderr, newValueURL) {
		t.Fatalf("stderr does not carry the link:\n%s", stderr)
	}
	if strings.Contains(stdout, newValueURL) || stdout != notCheckedLine {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestSecretNewValueJSON(t *testing.T) {
	fastNewValue(t, 5*time.Second, 5*time.Second)
	fakeBrowser(t)
	newNewValueFake(t, nvAnswer(),
		nvAnswer("valueChangedAt", changedT1, "lastCheckStatus", "auth_failed", "lastCheckError", "invalid API key", "lastCheckedAt", changedT1))
	stdout, _, errText := runNewValue(t, "--json", newValueID)
	if !strings.Contains(errText, "the service refused it") {
		t.Fatalf("error = %q, want the refusal to fail the command after the JSON", errText)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("--json stdout does not decode: %v\n%s", err, stdout)
	}
	if out["id"] != newValueID || out["checkStatus"] != "auth_failed" || out["checkError"] != "invalid API key" || out["valueChangedAt"] != changedT1 {
		t.Fatalf("json = %v", out)
	}
	if strings.Contains(stdout, newValueURL) {
		t.Fatalf("the link reached --json: %s", stdout)
	}
	assertNoForbiddenKeys(t, out)
}

func TestSecretNewValueJSONWorksExitsZero(t *testing.T) {
	fastNewValue(t, 5*time.Second, 5*time.Second)
	fakeBrowser(t)
	newNewValueFake(t, nvAnswer(),
		nvAnswer("valueChangedAt", changedT1, "lastCheckStatus", "ok", "lastCheckedAt", changedT1))
	stdout, _, errText := runNewValue(t, "--json", newValueID)
	if errText != "" {
		t.Fatalf("failed: %s", errText)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out["checkStatus"] != "ok" {
		t.Fatalf("json = %v (%v)\n%s", out, err, stdout)
	}
}

// Once the value changed, a failure to read the verdict must not lose that
// the new value is saved.
func TestSecretNewValueReadErrorDuringTheCheckWaitSaysSaved(t *testing.T) {
	fastNewValue(t, 5*time.Second, 5*time.Second)
	fakeBrowser(t)
	newNewValueFake(t, nvAnswer(),
		nvAnswer("valueChangedAt", changedT1),
		map[string]any{"httpStatus": 403, "error": "forbidden"})
	stdout, _, errText := runNewValue(t, "--json", newValueID)
	want := "the new value for secret " + newValueID + " is saved; reading Ronja's check failed: "
	if !strings.HasPrefix(errText, want) {
		t.Fatalf("error = %q, want it to start with %q", errText, want)
	}
	if stdout != "" {
		t.Fatalf("a failed read printed %q", stdout)
	}
}

// Ctrl-C after the value changed stops only the wait for the verdict.
func TestSecretNewValueCtrlCDuringTheCheckWait(t *testing.T) {
	fastNewValue(t, 5*time.Second, 5*time.Second)
	fakeBrowser(t)
	f := newNewValueFake(t, nvAnswer(), nvAnswer("valueChangedAt", changedT1))
	f.onRead = func(n int) {
		// Read 3 is the first one of the verdict wait.
		if n == 3 {
			if self, err := os.FindProcess(os.Getpid()); err == nil {
				_ = self.Signal(os.Interrupt)
			}
		}
	}
	stdout, _, errText := runNewValue(t, newValueID)
	want := "stopped waiting; the new value for secret " + newValueID + " is saved"
	if !strings.HasPrefix(errText, want) {
		t.Fatalf("error = %q, want it to start with %q", errText, want)
	}
	if stdout != "" {
		t.Fatalf("Ctrl-C printed %q", stdout)
	}
}

// Saving the value that is already stored doesn't move valueChangedAt, so the
// wait says so up front and when it runs out.
func TestSecretNewValueSaysTheSameValueDoesNotCount(t *testing.T) {
	fastNewValue(t, 50*time.Millisecond, time.Second)
	fakeBrowser(t)
	newNewValueFake(t, nvAnswer("valueChangedAt", changedT0))
	_, stderr, errText := runNewValue(t, newValueID)
	const same = "saving the value it already has doesn't count as a new one"
	if !strings.Contains(errText, same) {
		t.Fatalf("error = %q, want it to contain %q", errText, same)
	}
	if !strings.Contains(stderr, same) {
		t.Fatalf("the opening lines lack %q:\n%s", same, stderr)
	}
}

// Only the on-save check's sources describe the new value: a probe, or a
// login secret's login (refresh). An observed stamp is real traffic, which
// can have used the old value.
func TestSecretNewValueReadsOnlyTheOnSaveCheck(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"observed", notCheckedLine},
		{"refresh", "Saved. Ronja tried the new value and it works.\n"},
		{"probe", "Saved. Ronja tried the new value and it works.\n"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			fastNewValue(t, 5*time.Second, 20*time.Millisecond)
			fakeBrowser(t)
			newNewValueFake(t, nvAnswer(),
				nvAnswer("valueChangedAt", changedT1, "lastCheckStatus", "ok", "lastCheckSource", tc.source, "lastCheckedAt", "2026-11-07T12:00:02Z"))
			stdout, stderr, errText := runNewValue(t, newValueID)
			if errText != "" {
				t.Fatalf("failed: %s\nstderr: %s", errText, stderr)
			}
			if stdout != tc.want {
				t.Fatalf("stdout = %q, want %q", stdout, tc.want)
			}
		})
	}
}
