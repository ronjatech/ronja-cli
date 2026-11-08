package api

import (
	"errors"
	"net/http"
	"testing"
)

// spaceAnswering is a client for Space space-dev on an instance that answers
// every request with status, body and, when echo is set, X-Ronja-Space: echo.
func spaceAnswering(t *testing.T, status int, body, echo string, seen *[]string) *Client {
	t.Helper()
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.Method+" "+r.URL.Path+" "+r.Header.Get(HeaderSpace))
		}
		if echo != "" {
			w.Header().Set(HeaderSpace, echo)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	c.Space, c.SpaceName, c.SpaceLatch = "space-dev", "Dev", &SpaceLatch{}
	return c
}

// A response to a request that named the Space is confirmed only by its echo:
// a handler's 4xx must echo too, 401, 403, 429 and 5xx need not, and the
// middleware's 409 and 400 are refusals of their own.
func TestASpaceRequestIsConfirmedByItsEcho(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body, echo   string
		want         SpaceErrorKind
		wantHTTPCode int
	}{
		{"200 echoing", 200, `{}`, "space-dev", 0, 0},
		{"200 without echo", 200, `{}`, "", SpaceUnconfirmed, 0},
		{"200 naming another Space", 200, `{}`, "space-main", SpaceUnconfirmed, 0},
		{"404 echoing", 404, `{"error":"not found"}`, "space-dev", 0, 404},
		{"404 without echo", 404, `{"error":"not found"}`, "", SpaceUnconfirmed, 0},
		{"409 name_taken without echo", 409, `{"error":"taken","code":"name_taken"}`, "", SpaceUnconfirmed, 0},
		{"422 without echo", 422, `{"error":"bad"}`, "", SpaceUnconfirmed, 0},
		{"409 space_unavailable", 409, `{"error":"cannot enter","code":"space_unavailable"}`, "", SpaceUnavailable, 0},
		{"400 space_not_selectable", 400, `{"error":"cannot choose","code":"space_not_selectable"}`, "", SpaceNotSelectable, 0},
		{"401 without echo", 401, `{"error":"unauthorized"}`, "", 0, 401},
		{"403 without echo", 403, `{"error":"forbidden","code":"insufficient_scope"}`, "", 0, 403},
		{"429 without echo", 429, `{"error":"slow down"}`, "", 0, 429},
		{"503 without echo", 503, `{"error":"down"}`, "", 0, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := spaceAnswering(t, tc.status, tc.body, tc.echo, nil)
			err := c.Do(t.Context(), http.MethodGet, "workflow/wf-1", nil, nil)
			var se *SpaceError
			if errors.As(err, &se) != (tc.want != 0) || (se != nil && se.Kind != tc.want) {
				t.Fatalf("err = %v, want kind %d", err, tc.want)
			}
			if got := StatusOf(err); got != tc.wantHTTPCode {
				t.Errorf("StatusOf = %d, want %d: an unconfirmed answer must not read as its status", got, tc.wantHTTPCode)
			}
		})
	}
}

// Once one response does not confirm the Space, every later request of every
// client sharing the latch is refused before it is sent.
func TestASpaceLatchSendsNothingFurther(t *testing.T) {
	var seen []string
	c := spaceAnswering(t, 200, `{}`, "", &seen)
	if err := c.Do(t.Context(), http.MethodGet, "space/space-dev", nil, nil); err == nil {
		t.Fatal("the unconfirmed read passed")
	}
	other := New(c.BaseURL, "test-token")
	other.Space, other.SpaceLatch = c.Space, c.SpaceLatch
	for _, client := range []*Client{c, other} {
		err := client.Do(t.Context(), http.MethodPost, "workflow", map[string]any{}, nil)
		var se *SpaceError
		if !errors.As(err, &se) || se.Kind != SpaceUnconfirmed || se.Path != "/api/v2/space/space-dev" {
			t.Fatalf("later request: %v, want the first one's refusal", err)
		}
	}
	if len(seen) != 1 {
		t.Fatalf("sent after the refusal: %v", seen)
	}
}

// DoRaw checks the echo of a request it named the client's Space on, as a
// preview call makes; a header the caller wrote (`ronja api -H`) is not
// checked.
func TestDoRawChecksTheSpaceItSent(t *testing.T) {
	c := spaceAnswering(t, 200, `{"rendered":true}`, "", nil)
	_, err := c.PreviewDataApp(t.Context(), "app-1", PreviewRequest{})
	var se *SpaceError
	if !errors.As(err, &se) || se.Kind != SpaceUnconfirmed {
		t.Fatalf("preview on an instance that does not echo: %v", err)
	}

	refused := spaceAnswering(t, 409, `{"error":"cannot enter","code":"space_unavailable"}`, "", nil)
	if _, err := refused.DoRaw(t.Context(), "GET", "/api/v2/workflow/wf-1", nil, nil, DefaultRawTimeout); !errors.As(err, &se) || se.Kind != SpaceUnavailable {
		t.Fatalf("raw 409 space_unavailable: %v", err)
	}

	raw := spaceAnswering(t, 200, `{}`, "", nil)
	header := http.Header{}
	header.Set(HeaderSpace, "space-other")
	resp, err := raw.DoRaw(t.Context(), "GET", "/api/v2/workflow/wf-1", header, nil, DefaultRawTimeout)
	if err != nil {
		t.Fatalf("a caller-set header was checked: %v", err)
	}
	_ = resp.Body.Close()
}
