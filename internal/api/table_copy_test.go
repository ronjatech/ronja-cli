package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type deadlineRecorder struct {
	base     http.RoundTripper
	deadline time.Time
	had      bool
}

func (d *deadlineRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	d.deadline, d.had = r.Context().Deadline()
	return d.base.RoundTrip(r)
}

// A copy may take the route's whole 80 s, so it is asked with the slow
// deadline; the 30 s read bound would cut it before the route can answer.
func TestCopyTableUsesTheSlowDeadline(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v2/feature/model/table-src/copy" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"table":{"id":"table-copy","name":"orders","featureID":"col-dev"},"rowsCopied":3,"outcome":"created"}`))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "test-token")
	rec := &deadlineRecorder{base: http.DefaultTransport}
	c.HTTP.Transport = rec

	noRows := false
	started := time.Now()
	out, err := c.CopyTable(context.Background(), "table-src", CopyTableInput{FeatureID: "col-dev", Rows: &noRows})
	if err != nil {
		t.Fatal(err)
	}
	if !rec.had || rec.deadline.Sub(started) < 100*time.Second {
		t.Fatalf("copy deadline = %v from now (set: %v), want the slow 120 s", rec.deadline.Sub(started), rec.had)
	}
	if out.Table.ID != "table-copy" || out.RowsCopied != 3 || out.Outcome != "created" {
		t.Fatalf("decoded %+v", out)
	}
	if body["featureID"] != "col-dev" || body["rows"] != false {
		t.Fatalf("body = %v", body)
	}
	if _, sent := body["name"]; sent {
		t.Fatalf("an empty name was sent: %v", body)
	}
}
