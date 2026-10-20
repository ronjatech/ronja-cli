package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/update"
	"github.com/spf13/cobra"
)

// The tests drive run() because PersistentPreRunE starts the check and run
// joins it; ExecuteContext alone would never reach the finish. `profile list`
// reaches RunE, talks to no server and prints on stdout.

var noticeCommand = []string{"profile", "list"}

// forceTerminals reaches "stdout is a terminal but stderr is not" without a pty.
func forceTerminals(t *testing.T, stdout, stderr bool) {
	t.Helper()
	previous := isTerminal
	isTerminal = func(f *os.File) bool {
		switch f {
		case os.Stdout:
			return stdout
		case os.Stderr:
			return stderr
		}
		return false
	}
	t.Cleanup(func() { isTerminal = previous })
}

// noticeEnv is the one state where the notice can print.
func noticeEnv(t *testing.T, m *fakeMirror, current string) {
	t.Helper()
	useMirror(t, m, current)
	forceTerminals(t, true, true)
	t.Setenv("CI", "")
	t.Setenv("RONJA_NO_UPDATE_CHECK", "")

	previous := currentExecutable
	exe := filepath.Join(t.TempDir(), "bin", "ronja")
	currentExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { currentExecutable = previous })
}

func runNotice(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Chdir(t.TempDir())
	stderr = captureStderr(t, func() {
		out, restore := captureStdout(t)
		code = run(args)
		stdout = restore(out)
	})
	return code, stdout, stderr
}

// expectedNotice uses the shipping formatter, so the wording lives in one place.
func expectedNotice(t *testing.T) string {
	t.Helper()
	return expectedNoticeFor(t, testLatestVersion)
}

func expectedNoticeFor(t *testing.T, latest string) string {
	t.Helper()
	return update.Notice(testCurrentVersion, latest, update.MethodSwap)
}

func assertNoNotice(t *testing.T, stderr string) {
	t.Helper()
	if strings.Contains(stderr, "— run:") {
		t.Errorf("a notice was printed and should not have been:\n%s", stderr)
	}
}

func seedState(t *testing.T, s update.State) {
	t.Helper()
	if err := update.SaveState(s); err != nil {
		t.Fatalf("seed the state file: %v", err)
	}
}

func loadState(t *testing.T) update.State {
	t.Helper()
	s, err := update.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return s
}

func assertNear(t *testing.T, field string, got, want time.Time) {
	t.Helper()
	if d := got.Sub(want); d > time.Minute || d < -time.Minute {
		t.Errorf("%s = %s, want ≈ %s (off by %s)", field, got, want, d)
	}
}

func TestUpdateNoticePrintsOnceOnAStaleCache(t *testing.T) {
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	writeProfile(t, "acme", "https://example.invalid", "tenant-1", "token")

	code, stdout, stderr := runNotice(t, noticeCommand...)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got, want := noticeLines(stderr), []string{expectedNotice(t)}; !equalLines(got, want) {
		t.Errorf("stderr notice lines = %q, want %q\nfull stderr:\n%s", got, want, stderr)
	}
	if !strings.HasSuffix(stderr, expectedNotice(t)+"\n") {
		t.Errorf("the notice is not the last line of stderr:\n%q", stderr)
	}

	t.Setenv("RONJA_NO_UPDATE_CHECK", "1")
	_, control, controlErr := runNotice(t, noticeCommand...)
	assertNoNotice(t, controlErr)
	if stdout != control {
		t.Errorf("stdout differs with the check on:\n got %q\nwant %q", stdout, control)
	}

	state := loadState(t)
	now := time.Now()
	assertNear(t, "CheckedAt", state.CheckedAt, now)
	assertNear(t, "NotifiedAt", state.NotifiedAt, now)
	if state.Latest != testLatestVersion {
		t.Errorf("state Latest = %q, want %q", state.Latest, testLatestVersion)
	}
	if n := mirror.requestCount(); n != 1 {
		t.Errorf("mirror saw %d requests, want 1", n)
	}
}

func TestUpdateNoticeStaysSilent(t *testing.T) {
	cases := []struct {
		name     string
		arrange  func(t *testing.T, m *fakeMirror) []string
		requests int
	}{{
		name: "a development build cannot be compared",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			Version = "dev"
			return noticeCommand
		},
	}, {
		name: "stdout is not a terminal",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			forceTerminals(t, false, true)
			return noticeCommand
		},
	}, {
		name: "stderr is not a terminal",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			forceTerminals(t, true, false)
			return noticeCommand
		},
	}, {
		name: "CI is set",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			t.Setenv("CI", "1")
			return noticeCommand
		},
	}, {
		name: "RONJA_NO_UPDATE_CHECK is set",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			t.Setenv("RONJA_NO_UPDATE_CHECK", "1")
			return noticeCommand
		},
	}, {
		name: "--json means a machine is reading",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			return append(append([]string{}, noticeCommand...), "--json")
		},
	}, {
		name: "env feeds eval",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			return []string{"env"}
		},
	}, {
		// The mirror sees update's own release lookup, not the check's.
		name:     "update reports for itself",
		requests: 1,
		arrange: func(t *testing.T, m *fakeMirror) []string {
			installedBinary(t, t.TempDir())
			return []string{"update", "--check"}
		},
	}, {
		name: "help is read as a whole",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			return []string{"help"}
		},
	}, {
		name: "completion is eval'd",
		arrange: func(t *testing.T, m *fakeMirror) []string {
			return []string{"completion", "bash"}
		},
	}, {
		// The cache is stale, so the fetch still runs.
		name:     "already notified an hour ago",
		requests: 1,
		arrange: func(t *testing.T, m *fakeMirror) []string {
			seedState(t, update.State{NotifiedAt: time.Now().Add(-time.Hour)})
			return noticeCommand
		},
	}, {
		name:     "the latest release is the running one",
		requests: 1,
		arrange: func(t *testing.T, m *fakeMirror) []string {
			m.tag = testCurrentVersion
			return noticeCommand
		},
	}, {
		name:     "the latest release is older than the running one",
		requests: 1,
		arrange: func(t *testing.T, m *fakeMirror) []string {
			m.tag = "v0.27.0"
			return noticeCommand
		},
	}, {
		name:     "the mirror answers 500",
		requests: 1,
		arrange: func(t *testing.T, m *fakeMirror) []string {
			m.failRelease = true
			return noticeCommand
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
			noticeEnv(t, mirror, testCurrentVersion)
			args := tc.arrange(t, mirror)

			_, _, stderr := runNotice(t, args...)
			assertNoNotice(t, stderr)
			if n := mirror.requestCount(); n != tc.requests {
				t.Errorf("mirror saw %d requests, want %d", n, tc.requests)
			}
		})
	}
}

func TestUpdateNoticeKeepsCheckedAtAfterAFailedLookup(t *testing.T) {
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	mirror.failRelease = true
	noticeEnv(t, mirror, testCurrentVersion)

	if _, _, stderr := runNotice(t, noticeCommand...); strings.Contains(stderr, "— run:") {
		t.Errorf("a failed lookup printed something:\n%s", stderr)
	}
	assertNear(t, "CheckedAt", loadState(t).CheckedAt, time.Now())
}

func TestUpdateNoticeSkipsEverythingWhenTheStateIsUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0500 directory")
	}
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)

	dir := os.Getenv("RONJA_CONFIG_DIR")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, _, stderr := runNotice(t, noticeCommand...)
	assertNoNotice(t, stderr)
	if n := mirror.requestCount(); n != 0 {
		t.Errorf("mirror saw %d requests, want 0 — an unwritable state must not reach the network", n)
	}
}

func TestUpdateNoticeSkipsEverythingWhenTheStateIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 file")
	}
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	seedState(t, update.State{CheckedAt: time.Now().Add(-25 * time.Hour)})

	path := filepath.Join(os.Getenv("RONJA_CONFIG_DIR"), update.StateFileName)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, _, stderr := runNotice(t, noticeCommand...)
	assertNoNotice(t, stderr)
	if n := mirror.requestCount(); n != 0 {
		t.Errorf("mirror saw %d requests, want 0 — an unreadable state must not reach the network", n)
	}
}

// `sudo ronja ...` on somebody else's config directory writes nothing
// (update.ErrStateNotOwned), so it must also fetch nothing.
func TestUpdateNoticeSkipsEverythingUnderSudo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no uid to compare — the refusal cannot trigger here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, so the config dir really is root's own")
	}
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)

	previous := update.Geteuid
	update.Geteuid = func() int { return 0 }
	t.Cleanup(func() { update.Geteuid = previous })

	_, _, stderr := runNotice(t, noticeCommand...)
	assertNoNotice(t, stderr)
	if n := mirror.requestCount(); n != 0 {
		t.Errorf("mirror saw %d requests, want 0 — a refused state write must not reach the network", n)
	}
}

func TestUpdateNoticeSurvivesAnUnparseableAnswer(t *testing.T) {
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>proxy sign-in required</html>"))
	}))
	t.Cleanup(garbage.Close)

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	releaseBaseURL = garbage.URL

	_, _, stderr := runNotice(t, noticeCommand...)
	assertNoNotice(t, stderr)

	state := loadState(t)
	assertNear(t, "CheckedAt", state.CheckedAt, time.Now())
	if state.Latest != "" {
		t.Errorf("state Latest = %q, want empty — nothing was learned", state.Latest)
	}
}

// Runs against the seam-shortened timeout; the real bound has its own test.
func TestUpdateNoticeBacksOffAnHourAfterATimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	releaseBaseURL = slow.URL
	previous := updateCheckFetchTimeout
	updateCheckFetchTimeout = 20 * time.Millisecond
	t.Cleanup(func() { updateCheckFetchTimeout = previous })

	_, _, stderr := runNotice(t, noticeCommand...)
	assertNoNotice(t, stderr)
	assertNear(t, "CheckedAt", loadState(t).CheckedAt, time.Now().Add(-updateCheckTimeoutBackdate))
}

func TestUpdateNoticeIsBoundedByOneSecond(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	t.Cleanup(stall.Close)

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	releaseBaseURL = stall.URL

	start := time.Now()
	_, _, stderr := runNotice(t, noticeCommand...)
	elapsed := time.Since(start)

	assertNoNotice(t, stderr)
	if elapsed > 1500*time.Millisecond {
		t.Errorf("run() took %s, want under 1.5s — the fetch deadline did not bound it", elapsed)
	}
	// A check that never started also passes the upper bound; this lower bound
	// proves the real deadline fired.
	if elapsed < 800*time.Millisecond {
		t.Errorf("run() took %s — too fast for a one-second deadline to have been waited on", elapsed)
	}
	assertNear(t, "CheckedAt", loadState(t).CheckedAt, time.Now().Add(-updateCheckTimeoutBackdate))
}

// Driven through the two halves directly, because cancelling run's own signal
// context would mean signalling the test binary. The real killed fetch proves
// its error still matches context.Canceled through net/http's wrapper.
func TestUpdateNoticeBacksOffAnHourAfterACancellation(t *testing.T) {
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)

	root := newRootCmd()
	cmd, _, err := root.Find(noticeCommand)
	if err != nil {
		t.Fatalf("find %v: %v", noticeCommand, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := startUpdateCheck(ctx, cmd)
	if c == nil || c.result == nil {
		t.Fatal("no fetch was started — the cancellation branch is unreachable")
	}
	stderr := captureStderr(t, func() { finishUpdateCheck(c) })

	assertNoNotice(t, stderr)
	assertNear(t, "CheckedAt", loadState(t).CheckedAt, time.Now().Add(-updateCheckTimeoutBackdate))
}

// An argv scan read `ronja --profile acme update` as command "acme".
func TestUpdateNoticeIsNotFooledByValuedGlobalFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--profile", "acme", "update", "--check"},
		{"--profile", "acme", "help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
			noticeEnv(t, mirror, testCurrentVersion)
			installedBinary(t, t.TempDir())

			_, _, stderr := runNotice(t, args...)
			assertNoNotice(t, stderr)
		})
	}
}

// The check may have happened in a shell that could not print.
func TestUpdateNoticePrintsFromACacheWithoutFetching(t *testing.T) {
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	seedState(t, update.State{CheckedAt: time.Now().Add(-time.Hour), Latest: testLatestVersion})

	_, _, stderr := runNotice(t, noticeCommand...)
	if got, want := noticeLines(stderr), []string{expectedNotice(t)}; !equalLines(got, want) {
		t.Errorf("stderr notice lines = %q, want %q", got, want)
	}
	if n := mirror.requestCount(); n != 0 {
		t.Errorf("mirror saw %d requests, want 0 — the cache was fresh", n)
	}
}

// CheckedAt on disk before the mirror answers is the multi-process guard.
func TestUpdateNoticeClaimsTheCheckBeforeFetching(t *testing.T) {
	var sawCheckedAt atomic.Bool
	var requests atomic.Int32

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	seedState(t, update.State{CheckedAt: time.Now().Add(-25 * time.Hour)})
	stale := loadState(t).CheckedAt

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if s, err := update.LoadState(); err == nil && s.CheckedAt.After(stale) {
			sawCheckedAt.Store(true)
		}
		w.Write(mirror.releaseJSON())
	}))
	t.Cleanup(fake.Close)
	releaseBaseURL = fake.URL

	_, _, stderr := runNotice(t, noticeCommand...)
	if got, want := noticeLines(stderr), []string{expectedNotice(t)}; !equalLines(got, want) {
		t.Errorf("stderr notice lines = %q, want %q", got, want)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("the fake saw %d requests, want 1", n)
	}
	if !sawCheckedAt.Load() {
		t.Error("CheckedAt was not on disk when the mirror was asked")
	}
}

func TestUpdateNoticeFetchKeepsNotifiedAt(t *testing.T) {
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	notified := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	seedState(t, update.State{NotifiedAt: notified})

	_, _, stderr := runNotice(t, noticeCommand...)
	assertNoNotice(t, stderr)

	state := loadState(t)
	if !state.NotifiedAt.Equal(notified) {
		t.Errorf("NotifiedAt = %s, want the seeded %s to survive", state.NotifiedAt, notified)
	}
	if state.Latest != testLatestVersion {
		t.Errorf("state Latest = %q, want %q", state.Latest, testLatestVersion)
	}
}

func TestUpdateNoticeDoesNotRewindAConcurrentWrite(t *testing.T) {
	const newerRelease = "v0.30.0"
	concurrent := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)

	// The window: this run has read the cache and not yet written it back.
	var wrote atomic.Bool
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wrote.Swap(true) {
			if err := update.SaveState(update.State{CheckedAt: concurrent, Latest: newerRelease}); err != nil {
				t.Errorf("the concurrent write failed: %v", err)
			}
		}
		writeJSON(w, map[string]any{
			"user":   map[string]any{"id": "usr-1", "email": "dev@example.com"},
			"role":   map[string]any{"name": "user", "privilegeLevel": 50},
			"tenant": map[string]any{"id": "ten-test", "name": "Test Org"},
		})
	}))
	t.Cleanup(instance.Close)

	// signInTo takes a fresh config dir, so seed the cache after it.
	signInTo(t, instance.URL)
	seedState(t, update.State{CheckedAt: time.Now().Add(-time.Hour), Latest: testLatestVersion})

	// A cached run announces the file's Latest as it stands at the end.
	_, _, stderr := runNotice(t, "whoami")
	if got, want := noticeLines(stderr), []string{expectedNoticeFor(t, newerRelease)}; !equalLines(got, want) {
		t.Errorf("stderr notice lines = %q, want %q\nfull stderr:\n%s", got, want, stderr)
	}
	if !wrote.Load() {
		t.Fatal("the instance was never called — nothing wrote while the command ran")
	}
	if n := mirror.requestCount(); n != 0 {
		t.Errorf("mirror saw %d requests, want 0 — the cache was fresh", n)
	}

	state := loadState(t)
	if !state.CheckedAt.Equal(concurrent) {
		t.Errorf("CheckedAt = %s, want the concurrent %s — the notice rewound it", state.CheckedAt, concurrent)
	}
	if state.Latest != newerRelease {
		t.Errorf("Latest = %q, want the concurrent %q — the notice rewound it", state.Latest, newerRelease)
	}
	assertNear(t, "NotifiedAt", state.NotifiedAt, time.Now())
}

func TestUpdateNoticeKeepsAConcurrentLatestAfterAFailedFetch(t *testing.T) {
	const newerRelease = "v0.30.0"
	concurrent := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)

	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)
	// With a Latest already known a notice is due, which is what makes the run write.
	seedState(t, update.State{CheckedAt: time.Now().Add(-25 * time.Hour), Latest: testLatestVersion})

	var wrote atomic.Bool
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wrote.Swap(true) {
			if err := update.SaveState(update.State{CheckedAt: concurrent, Latest: newerRelease}); err != nil {
				t.Errorf("the concurrent write failed: %v", err)
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	releaseBaseURL = failing.URL

	_, _, stderr := runNotice(t, noticeCommand...)
	if got, want := noticeLines(stderr), []string{expectedNoticeFor(t, newerRelease)}; !equalLines(got, want) {
		t.Errorf("stderr notice lines = %q, want %q\nfull stderr:\n%s", got, want, stderr)
	}
	if !wrote.Load() {
		t.Fatal("the mirror was never asked — nothing wrote while the command ran")
	}

	state := loadState(t)
	if state.Latest != newerRelease {
		t.Errorf("Latest = %q, want the concurrent %q — a failed fetch rewound it", state.Latest, newerRelease)
	}
	if !state.CheckedAt.Equal(concurrent) {
		t.Errorf("CheckedAt = %s, want the concurrent %s — a failed fetch rewound it", state.CheckedAt, concurrent)
	}
	assertNear(t, "NotifiedAt", state.NotifiedAt, time.Now())
}

func TestUpdateNoticeReadsNotifiedAtAfterTheCommand(t *testing.T) {
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)

	var wrote atomic.Bool
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wrote.Swap(true) {
			if err := update.SaveState(update.State{
				CheckedAt:  time.Now(),
				NotifiedAt: time.Now(),
				Latest:     testLatestVersion,
			}); err != nil {
				t.Errorf("the concurrent write failed: %v", err)
			}
		}
		writeJSON(w, map[string]any{
			"user":   map[string]any{"id": "usr-1", "email": "dev@example.com"},
			"role":   map[string]any{"name": "user", "privilegeLevel": 50},
			"tenant": map[string]any{"id": "ten-test", "name": "Test Org"},
		})
	}))
	t.Cleanup(instance.Close)

	// signInTo takes a fresh config dir, so seed the cache after it.
	signInTo(t, instance.URL)
	seedState(t, update.State{CheckedAt: time.Now().Add(-time.Hour), Latest: testLatestVersion})

	_, _, stderr := runNotice(t, "whoami")
	assertNoNotice(t, stderr)
	if !wrote.Load() {
		t.Fatal("the instance was never called — nothing wrote while the command ran")
	}
	if n := mirror.requestCount(); n != 0 {
		t.Errorf("mirror saw %d requests, want 0 — the cache was fresh", n)
	}
}

func TestUpdateNoticeTreatsAFutureTimestampAsStale(t *testing.T) {
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)

	now := time.Now()
	previous := updateCheckNow
	updateCheckNow = func() time.Time { return now }
	t.Cleanup(func() { updateCheckNow = previous })

	ahead := now.Add(2 * time.Hour)
	seedState(t, update.State{CheckedAt: ahead, NotifiedAt: ahead, Latest: "v0.1.0"})

	_, _, stderr := runNotice(t, noticeCommand...)
	if got, want := noticeLines(stderr), []string{expectedNotice(t)}; !equalLines(got, want) {
		t.Errorf("stderr notice lines = %q, want %q\nfull stderr:\n%s", got, want, stderr)
	}
	if n := mirror.requestCount(); n != 1 {
		t.Errorf("mirror saw %d requests, want 1 — a future CheckedAt kept the fetch shut", n)
	}
	if state := loadState(t); state.Latest != testLatestVersion {
		t.Errorf("state Latest = %q, want %q", state.Latest, testLatestVersion)
	}
}

// cobra answers --version and an unknown command before any hook, so run()
// joins a check that never started.
func TestUpdateNoticeSurvivesACommandThatNeverReachesThePreRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"--version answers before the hooks", []string{"--version"}},
		{"an unknown command never gets that far", []string{"no-such-command"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
			noticeEnv(t, mirror, testCurrentVersion)

			_, _, stderr := runNotice(t, tc.args...)
			assertNoNotice(t, stderr)
			if pendingUpdateCheck != nil {
				t.Errorf("a check was started for %v", tc.args)
			}
			if n := mirror.requestCount(); n != 0 {
				t.Errorf("mirror saw %d requests, want 0", n)
			}
		})
	}
}

func TestUpdateNoticeLeavesAFailingCommandAlone(t *testing.T) {
	mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
	noticeEnv(t, mirror, testCurrentVersion)

	code, _, stderr := runNotice(t, "profile", "use", "no-such-profile")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if n := strings.Count(stderr, "error:"); n != 1 {
		t.Errorf("stderr carries %d error lines, want 1:\n%s", n, stderr)
	}
	if !strings.HasSuffix(stderr, expectedNotice(t)+"\n") {
		t.Errorf("the notice does not follow the error line:\n%q", stderr)
	}
}

func TestUpdateNoticeNamesTheInstallMethod(t *testing.T) {
	cases := []struct {
		name   string
		exe    string
		method update.Method
	}{
		{"a cask install defers to brew", "/opt/homebrew/Caskroom/ronja/0.28.4/ronja", update.MethodHomebrew},
		{"a plain file swaps itself", "/usr/local/bin/ronja", update.MethodSwap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mirror := newFakeMirror(t, testLatestVersion, releaseTarball(t))
			noticeEnv(t, mirror, testCurrentVersion)
			previous := currentExecutable
			currentExecutable = func() (string, error) { return tc.exe, nil }
			t.Cleanup(func() { currentExecutable = previous })

			_, _, stderr := runNotice(t, noticeCommand...)
			want := update.Notice(testCurrentVersion, testLatestVersion, tc.method)
			if got := noticeLines(stderr); !equalLines(got, []string{want}) {
				t.Errorf("stderr notice lines = %q, want %q", got, want)
			}
		})
	}
}

// cobra runs only the NEAREST PersistentPreRun, so one on any subcommand would
// silently switch the check off for that branch.
func TestNoSubcommandShadowsTheRootPersistentPreRun(t *testing.T) {
	root := newRootCmd()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			if child.PersistentPreRun != nil || child.PersistentPreRunE != nil {
				t.Errorf("%q defines its own PersistentPreRun — it shadows the root's update check", child.CommandPath())
			}
			walk(child)
		}
	}
	walk(root)
}

// noticeLines keeps a command's own output from passing or failing an assertion.
func noticeLines(stderr string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "— run:") {
			out = append(out, line)
		}
	}
	return out
}

func equalLines(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
