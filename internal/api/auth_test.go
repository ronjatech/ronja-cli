package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestClient points a client at a stub server and collapses the poll timing,
// so the tests exercise the state machine rather than the clock.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "")
	c.MinPollInterval = time.Millisecond
	c.PollBackoff = time.Millisecond
	c.MaxPollInterval = 5 * time.Millisecond
	// Collapsed too, or every give-up test would sit out the real 90-second
	// tolerance. Still far longer than the sub-millisecond loopback round trips
	// below, so a genuine multi-failure run is never cut short by it.
	c.MaxPollFailureWindow = 100 * time.Millisecond
	c.RateLimitBackoff = time.Millisecond
	return c
}

// fastStart is a device-start response for tests; the client's collapsed
// MinPollInterval is what actually governs the cadence.
func fastStart(deviceCode string) *DeviceStart {
	return &DeviceStart{DeviceCode: deviceCode, Interval: 0, ExpiresIn: 30}
}

func TestPollDeviceSucceedsAfterPending(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": ErrAuthorizationPending})
			return
		}
		json.NewEncoder(w).Encode(DeviceToken{Token: "pat-abc", UserID: "user-1"})
	})

	token, err := client.PollDevice(context.Background(), fastStart("dev-code"))
	if err != nil {
		t.Fatalf("PollDevice: %v", err)
	}
	if token.Token != "pat-abc" {
		t.Errorf("token = %q, want pat-abc", token.Token)
	}
	if calls != 3 {
		t.Errorf("polled %d times, want 3", calls)
	}
}

func TestPollDeviceTerminalOutcomes(t *testing.T) {
	tests := []struct {
		name string
		code string
		want error
	}{
		{"denied", ErrAccessDenied, ErrDeviceDenied},
		{"expired", ErrExpiredToken, ErrDeviceExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": tt.code})
			})
			_, err := client.PollDevice(context.Background(), fastStart("dev-code"))
			if !errors.Is(err, tt.want) {
				t.Fatalf("PollDevice error = %v, want %v", err, tt.want)
			}
		})
	}
}

// A second poll after the token was already collected must not look like a
// transient failure — the flow is over and the user has to start again.
func TestPollDeviceInvalidGrantIsFatal(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": ErrInvalidGrant})
	})
	_, err := client.PollDevice(context.Background(), fastStart("dev-code"))
	if err == nil {
		t.Fatal("expected an error for invalid_grant")
	}
	if errors.Is(err, ErrDeviceDenied) || errors.Is(err, ErrDeviceExpired) {
		t.Fatalf("invalid_grant must not be reported as denial or expiry, got %v", err)
	}
}

// slow_down backs the client off instead of aborting; the login must still
// complete.
func TestPollDeviceBacksOffOnSlowDown(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": ErrSlowDown})
			return
		}
		json.NewEncoder(w).Encode(DeviceToken{Token: "pat-xyz"})
	})

	if _, err := client.PollDevice(context.Background(), fastStart("dev-code")); err != nil {
		t.Fatalf("PollDevice: %v", err)
	}
	if calls != 2 {
		t.Errorf("polled %d times, want 2 (one slow_down, then success)", calls)
	}
}

func TestPollDeviceHonoursContextCancellation(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": ErrAuthorizationPending})
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.PollDevice(ctx, fastStart("dev-code")); err == nil {
		t.Fatal("expected cancellation to stop polling")
	}
}

// A PERSISTENT server failure must eventually surface, not be mistaken for
// "still waiting" — otherwise a broken backend looks like a user who never
// clicks. It is bounded by MaxPollFailureWindow rather than fatal on the first
// one; see TestPollDeviceRidesOutTransientFailures.
func TestPollDeviceSurfacesPersistentServerErrors(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "server_error"})
	})
	started := time.Now()
	_, err := client.PollDevice(context.Background(), fastStart("dev-code"))
	if err == nil {
		t.Fatal("expected a persistent server error to abort the poll")
	}
	if StatusOf(err) != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (the real cause must stay reachable)", StatusOf(err))
	}
	// The bound is elapsed time, so the exact call count is a function of the
	// cadence and not worth pinning. What matters is that it RETRIED (a single
	// blip must not burn a login) and that it eventually stopped.
	if calls < 2 {
		t.Errorf("polled %d times, want at least 2 — a single 5xx must not be fatal", calls)
	}
	if elapsed := time.Since(started); elapsed > 20*client.MaxPollFailureWindow {
		t.Errorf("took %s to give up, want roughly MaxPollFailureWindow (%s)", elapsed, client.MaxPollFailureWindow)
	}
}

// A blip must not burn the login. A rolling deploy (5xx), the global rate
// limiter (429) and a dropped connection all happen routinely inside the ten
// minutes a device flow is alive, and the human on the other end has done
// nothing wrong.
func TestPollDeviceRidesOutTransientFailures(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "server_error"})
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "rate limited"})
		case 3:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": ErrAuthorizationPending})
		default:
			json.NewEncoder(w).Encode(DeviceToken{Token: "pat-survived"})
		}
	})

	token, err := client.PollDevice(context.Background(), fastStart("dev-code"))
	if err != nil {
		t.Fatalf("PollDevice must survive transient failures, got %v", err)
	}
	if token.Token != "pat-survived" {
		t.Errorf("token = %q, want pat-survived", token.Token)
	}
}

// The consecutive-failure budget resets on any clean answer, so a flow that
// blips repeatedly but keeps recovering is not eventually killed by the sum of
// unrelated hiccups.
func TestPollDeviceResetsFailureBudgetOnProgress(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Fail every other poll, forever — never a long enough unbroken run.
		if calls%2 == 1 && calls < 20 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if calls < 20 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": ErrAuthorizationPending})
			return
		}
		json.NewEncoder(w).Encode(DeviceToken{Token: "pat-eventually"})
	})

	token, err := client.PollDevice(context.Background(), fastStart("dev-code"))
	if err != nil {
		t.Fatalf("PollDevice: %v", err)
	}
	if token.Token != "pat-eventually" {
		t.Errorf("token = %q, want pat-eventually", token.Token)
	}
}

// A 4xx that is not part of the RFC vocabulary is the server rejecting the
// REQUEST, which retrying cannot fix.
func TestPollDeviceDoesNotRetryClientErrors(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := client.PollDevice(context.Background(), fastStart("dev-code")); err == nil {
		t.Fatal("expected a 404 to abort the poll")
	}
	if calls != 1 {
		t.Errorf("polled %d times, want 1 (a 404 is not transient)", calls)
	}
}

// The first poll happens IMMEDIATELY. The server's throttle always passes a
// first poll (last_polled_at IS NULL), so sleeping up front added a fixed
// several-second delay to every single login for nothing.
func TestPollDevicePollsBeforeSleeping(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DeviceToken{Token: "pat-instant"})
	})
	client.MinPollInterval = 30 * time.Second

	started := time.Now()
	if _, err := client.PollDevice(context.Background(), fastStart("dev-code")); err != nil {
		t.Fatalf("PollDevice: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("first poll waited %v; it must not sleep before polling", elapsed)
	}
}

// slow_down must not compound without limit: an uncapped monotonic climb would
// eventually poll less often than the flow has life left.
func TestPollDeviceCapsSlowDownBackoff(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 10 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": ErrSlowDown})
			return
		}
		json.NewEncoder(w).Encode(DeviceToken{Token: "pat-capped"})
	})
	client.PollBackoff = 50 * time.Millisecond
	client.MaxPollInterval = 60 * time.Millisecond

	started := time.Now()
	if _, err := client.PollDevice(context.Background(), fastStart("dev-code")); err != nil {
		t.Fatalf("PollDevice: %v", err)
	}
	// Uncapped, ten slow_downs would sum to 50+100+…+500ms = 2.75s.
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Errorf("ten slow_downs took %v; the backoff is not capped", elapsed)
	}
}

func TestDoSendsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(Me{})
	}))
	t.Cleanup(srv.Close)

	if _, err := New(srv.URL, "tok-123").Me(context.Background()); err != nil {
		t.Fatalf("Me: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", gotAuth)
	}
}

func TestDoBuildsV2Paths(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(Me{})
	}))
	t.Cleanup(srv.Close)

	if _, err := New(srv.URL+"/", "").Me(context.Background()); err != nil {
		t.Fatalf("Me: %v", err)
	}
	// A trailing slash on the base URL must not produce a doubled separator.
	if gotPath != "/api/v2/authentication/me" {
		t.Errorf("path = %q, want /api/v2/authentication/me", gotPath)
	}
}

// A 429 is the global rate limiter, and it is the one transient failure where
// retrying at the same cadence is actively wrong: the server is reporting too
// much traffic, and an unchanged cadence promises to keep producing it. Every
// other transient failure resumes the server's advertised interval.
func TestPollDeviceBacksOffOnRateLimit(t *testing.T) {
	calls := 0
	var gaps []time.Duration
	last := time.Time{}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !last.IsZero() {
			gaps = append(gaps, time.Since(last))
		}
		last = time.Now()
		if calls <= 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(DeviceToken{Token: "pat-after-429"})
	})
	// Room for the backoff to actually climb, and a window wide enough that the
	// climbing cadence does not trip the give-up bound.
	client.MaxPollInterval = 100 * time.Millisecond
	client.RateLimitBackoff = 10 * time.Millisecond
	client.MaxPollFailureWindow = 5 * time.Second

	token, err := client.PollDevice(context.Background(), fastStart("dev-code"))
	if err != nil {
		t.Fatalf("PollDevice: %v", err)
	}
	if token.Token != "pat-after-429" {
		t.Errorf("token = %q, want pat-after-429", token.Token)
	}
	if len(gaps) < 2 {
		t.Fatalf("expected at least two inter-poll gaps, got %d", len(gaps))
	}
	// Each successive 429 must widen the gap; without backoff they would all sit
	// at the collapsed MinPollInterval.
	if gaps[1] <= gaps[0] {
		t.Errorf("gaps did not widen across consecutive 429s: %v then %v", gaps[0], gaps[1])
	}
}
