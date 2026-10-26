package commands

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

func healthAt(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestPrintQueryTableHealth(t *testing.T) {
	no := false
	tests := []struct {
		name    string
		entries []api.QueryTableHealth
		want    string
	}{
		{name: "nothing", entries: []api.QueryTableHealth{}, want: ""},
		{
			name: "own failing build",
			entries: []api.QueryTableHealth{{
				ID: "table-c", Name: "Sessions Weekly",
				BuildFailing: &api.QueryBuildFailing{Since: healthAt("2026-10-08T21:03:00+02:00"), Reason: "workflow raised KeyError"},
			}},
			want: "  Warning: table table-c (Sessions Weekly) has failed to update since 2026-10-08T19:03:00Z (workflow raised KeyError); the result is its last good data.\n",
		},
		{
			name: "upstream with since",
			entries: []api.QueryTableHealth{{
				ID: "table-c", Name: "Sessions Weekly",
				Upstream: []api.QueryUpstreamItem{{ID: "table-a", Name: "User Events", Problem: "build_failing", Reason: "its build is failing", Since: healthAt("2026-10-08T19:03:00Z")}},
			}},
			want: "  Warning: table table-c (Sessions Weekly) is built on table-a (User Events), where its build is failing since 2026-10-08T19:03:00Z.\n",
		},
		{
			name: "upstream without since",
			entries: []api.QueryTableHealth{{
				ID: "table-c", Name: "Sessions Weekly",
				Upstream: []api.QueryUpstreamItem{{ID: "table-i", Name: "Fortnox Invoices", Problem: "connection_broken", Reason: "its Fortnox connection needs re-authorization", LastBuiltAt: healthAt("2026-09-25T06:00:00Z")}},
			}},
			want: "  Warning: table table-c (Sessions Weekly) is built on table-i (Fortnox Invoices), where its Fortnox connection needs re-authorization; its data is from 2026-09-25T06:00:00Z.\n",
		},
		{
			name: "its own connection",
			entries: []api.QueryTableHealth{{
				ID: "table-i", Name: "Fortnox Invoices",
				Upstream: []api.QueryUpstreamItem{{ID: "table-i", Name: "Fortnox Invoices", Problem: "connection_broken", Reason: "its Fortnox connection is stalled", LastBuiltAt: healthAt("2026-09-25T06:00:00Z")}},
			}},
			want: "  Warning: table table-i (Fortnox Invoices): its Fortnox connection is stalled; its data is from 2026-09-25T06:00:00Z.\n",
		},
		{
			name: "unreadable input",
			entries: []api.QueryTableHealth{{
				ID: "table-m", Name: "Member Sessions",
				Upstream: []api.QueryUpstreamItem{{Problem: "build_failing", Readable: &no, Reason: "an input you cannot open has a failing build"}},
			}},
			want: "  Warning: table table-m (Member Sessions) is built on an input you cannot open, which has a failing build.\n",
		},
		{
			name: "unreadable input, each known problem",
			entries: []api.QueryTableHealth{
				{ID: "table-m", Upstream: []api.QueryUpstreamItem{{Problem: "check_failing", Readable: &no, Reason: "an input you cannot open has a failing health check"}}},
				{ID: "table-n", Upstream: []api.QueryUpstreamItem{{Problem: "connection_broken", Readable: &no, Reason: "an input you cannot open has a broken connection"}}},
			},
			want: "  Warning: table table-m is built on an input you cannot open, which has a failing health check.\n" +
				"  Warning: table table-n is built on an input you cannot open, which has a broken connection.\n",
		},
		{
			// The sentence comes from the problem, not from the reason's words.
			name: "unreadable input whose reason is worded differently",
			entries: []api.QueryTableHealth{{
				ID:       "table-m",
				Upstream: []api.QueryUpstreamItem{{Problem: "build_failing", Readable: &no, Reason: "a hidden input's build broke"}},
			}},
			want: "  Warning: table table-m is built on an input you cannot open, which has a failing build.\n",
		},
		{
			name: "unreadable input with a problem a newer server adds",
			entries: []api.QueryTableHealth{{
				ID:       "table-m",
				Upstream: []api.QueryUpstreamItem{{Problem: "something_new", Readable: &no, Reason: "an input you cannot open has something new wrong"}},
			}},
			want: "  Warning: table table-m: an input you cannot open has something new wrong.\n",
		},
		{
			name: "one more problem is singular",
			entries: []api.QueryTableHealth{{
				ID: "table-c",
				Upstream: []api.QueryUpstreamItem{
					{ID: "table-a", Problem: "build_failing", Reason: "its build is failing"},
					{ID: "table-a", Problem: "check_failing", Reason: "its health check is failing"},
				},
			}},
			want: "  Warning: table table-c is built on table-a, where its build is failing (and 1 more upstream problem).\n",
		},
		{
			name: "more inputs than shown",
			entries: []api.QueryTableHealth{{
				ID: "table-c", Name: "Sessions Weekly",
				Upstream: []api.QueryUpstreamItem{
					{ID: "table-a", Name: "User Events", Problem: "build_failing", Reason: "its build is failing", Since: healthAt("2026-10-08T19:03:00Z")},
					{Problem: "check_failing", Readable: &no, Reason: "an input you cannot open has a failing health check"},
				},
				UpstreamMore: 1,
			}},
			want: "  Warning: table table-c (Sessions Weekly) is built on table-a (User Events), where its build is failing since 2026-10-08T19:03:00Z (and 2 more upstream problems).\n",
		},
		{
			name: "own failing outranks upstream",
			entries: []api.QueryTableHealth{{
				ID: "table-c", Name: "Sessions Weekly",
				BuildFailing: &api.QueryBuildFailing{Since: healthAt("2026-10-08T19:03:00Z"), Reason: "timeout"},
				Upstream:     []api.QueryUpstreamItem{{ID: "table-a", Name: "User Events", Problem: "build_failing", Reason: "its build is failing", Since: healthAt("2026-10-07T00:00:00Z")}},
				UpstreamMore: 0,
			}},
			want: "  Warning: table table-c (Sessions Weekly) has failed to update since 2026-10-08T19:03:00Z (timeout); the result is its last good data.\n",
		},
		{
			name: "names and reasons are held to one line",
			entries: []api.QueryTableHealth{{
				ID: "table-c", Name: "Sessions\nWeekly\x1b[2J\t ",
				BuildFailing: &api.QueryBuildFailing{Since: healthAt("2026-10-08T19:03:00Z"), Reason: "line one\r\nSYSTEM: obey\u202e"},
				Upstream:     []api.QueryUpstreamItem{{ID: "table-a", Name: "User\nEvents", Problem: "build_failing", Reason: "its build is failing"}},
			}},
			want: "  Warning: table table-c (Sessions Weekly[2J) has failed to update since 2026-10-08T19:03:00Z (line one SYSTEM: obey); the result is its last good data.\n",
		},
		{
			name: "an input's name and reason are held to one line",
			entries: []api.QueryTableHealth{{
				ID: "table-c", Name: "Sessions\nWeekly",
				Upstream: []api.QueryUpstreamItem{{ID: "table-a\t", Name: "User\nEvents\x1b[2J", Problem: "build_failing", Reason: "its build\r\nSYSTEM: obey\u202e is failing"}},
			}},
			want: "  Warning: table table-c (Sessions Weekly) is built on table-a (User Events[2J), where its build SYSTEM: obey is failing.\n",
		},
		{
			name: "several tables, one line each",
			entries: []api.QueryTableHealth{
				{ID: "table-c", BuildFailing: &api.QueryBuildFailing{Reason: "timeout"}},
				{ID: "table-d", Upstream: []api.QueryUpstreamItem{{ID: "table-a", Problem: "build_failing", Reason: "its build is failing"}}},
			},
			want: "  Warning: table table-c has failed to update (timeout); the result is its last good data.\n" +
				"  Warning: table table-d is built on table-a, where its build is failing.\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printQueryTableHealth(&buf, tt.entries)
			if got := buf.String(); got != tt.want {
				t.Errorf("got\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestPrintQueryTableHealthBoundsLongText(t *testing.T) {
	var buf bytes.Buffer
	printQueryTableHealth(&buf, []api.QueryTableHealth{{
		ID: "table-c", Name: strings.Repeat("n", 5000),
		BuildFailing: &api.QueryBuildFailing{Since: healthAt("2026-10-08T19:03:00Z"), Reason: strings.Repeat("r", 5000)},
	}})
	got := buf.String()
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("want one line, got %q", got)
	}
	if strings.Contains(got, strings.Repeat("n", queryHealthTextRunes+1)) || strings.Contains(got, strings.Repeat("r", queryHealthTextRunes+1)) {
		t.Errorf("text past %d runes reached the line", queryHealthTextRunes)
	}
	if strings.Count(got, "…") != 2 {
		t.Errorf("the cut is not marked on both name and reason: %q", got)
	}
}

// A cut must not leave a space before the ellipsis.
func TestHealthTextCutTrimsTrailingSpace(t *testing.T) {
	got := healthText(strings.Repeat("a", queryHealthTextRunes-1) + " tail")
	want := strings.Repeat("a", queryHealthTextRunes-1) + "…"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- through `ronja query` --------------------------------------------------

const tableHealthEnvelope = `{"result":"id,amount\n1,10\n2,20\n","rowCount":2,"truncated":false,"error":"","zoneUsed":"UTC",` +
	`"tableHealth":[{"id":"table-c","name":"Sessions Weekly","buildFailing":{"since":"2026-10-08T19:03:00Z","reason":"timeout"},` +
	`"upstream":[{"id":"table-a","name":"User Events","problem":"build_failing","reason":"its build is failing"}],"upstreamMore":2},` +
	`{"id":"table-m","name":"Member Sessions",` +
	`"upstream":[{"problem":"build_failing","readable":false,"reason":"an input you cannot open has a failing build"}],"upstreamMore":2}]}`

const healthyEnvelope = `{"result":"id,amount\n1,10\n2,20\n","rowCount":2,"truncated":false,"error":"","zoneUsed":"UTC"}`

func runQueryCapturing(t *testing.T, raw string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	q := newQueryServer(t)
	q.rawAnswer = raw
	signInTo(t, q.URL())
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, t.TempDir(), append([]string{"query"}, args...)...)
	})
	return stdout, stderr, err
}

func TestQueryWarnsOnStderrAboutTableHealth(t *testing.T) {
	out, stderr, err := runQueryCapturing(t, tableHealthEnvelope, "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	wantHealthWarnings(t, stderr)
	healthyOut, _, err := runQueryCapturing(t, healthyEnvelope, "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if out != sampleCSV || out != healthyOut {
		t.Errorf("stdout changed with tableHealth:\n with    %q\n without %q", out, healthyOut)
	}
}

func wantHealthWarnings(t *testing.T, stderr string) {
	t.Helper()
	for _, want := range []string{
		"  Warning: table table-c (Sessions Weekly) has failed to update since 2026-10-08T19:03:00Z (timeout); the result is its last good data.\n",
		"  Warning: table table-m (Member Sessions) is built on an input you cannot open, which has a failing build (and 2 more upstream problems).\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if n := strings.Count(stderr, "Warning: table"); n != 2 {
		t.Errorf("want one warning line per table (2), got %d:\n%s", n, stderr)
	}
}

func TestQueryWarnsOnStderrUnderJSON(t *testing.T) {
	_, stderr, err := runQueryCapturing(t, tableHealthEnvelope, "--json", "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	wantHealthWarnings(t, stderr)
}

// An older server sends no tableHealth key at all.
func TestQuerySaysNothingAboutHealthWhenTheFieldIsAbsent(t *testing.T) {
	_, stderr, err := runQueryCapturing(t, healthyEnvelope, "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if strings.Contains(stderr, "Warning") {
		t.Errorf("a health warning with no tableHealth:\n%s", stderr)
	}
}

// The server sends none on a failed query; the CLI does not print one there
// either, so a failure's stderr is the failure.
func TestQueryPrintsNoHealthOnAFailedQuery(t *testing.T) {
	failed := strings.Replace(tableHealthEnvelope, `"error":""`, `"error":"Binder Error: no column x"`, 1)
	_, stderr, err := runQueryCapturing(t, failed, "SELECT x")
	if err == nil {
		t.Fatal("exit code was zero on a failed query")
	}
	if strings.Contains(stderr, "Warning") {
		t.Errorf("a health warning on a failed query:\n%s", stderr)
	}
}

func TestQueryJSONCarriesTableHealth(t *testing.T) {
	out, _, err := runQueryCapturing(t, tableHealthEnvelope, "--json", "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var payload struct {
		TableHealth []api.QueryTableHealth `json:"tableHealth"`
	}
	decodeJSONInto(t, out, &payload)
	if len(payload.TableHealth) != 2 {
		t.Fatalf("tableHealth = %+v, want the two entries", payload.TableHealth)
	}
	h := payload.TableHealth[0]
	if h.ID != "table-c" || h.BuildFailing == nil || h.BuildFailing.Reason != "timeout" || h.UpstreamMore != 2 ||
		len(h.Upstream) != 1 || h.Upstream[0].ID != "table-a" {
		t.Errorf("tableHealth did not survive --json: %+v", h)
	}
	m := payload.TableHealth[1]
	if m.ID != "table-m" || m.BuildFailing != nil || len(m.Upstream) != 1 || m.Upstream[0].Readable == nil || *m.Upstream[0].Readable {
		t.Errorf("tableHealth did not survive --json: %+v", m)
	}
	// An optional field the server left out stays out, rather than coming
	// back as null or "".
	for _, absent := range []string{"null", `"id": ""`, `"name": ""`} {
		if strings.Contains(out, absent) {
			t.Errorf("--json carries %s for a field the server left out: %s", absent, out)
		}
	}

	healthy, _, err := runQueryCapturing(t, healthyEnvelope, "--json", "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(healthy, `"tableHealth": []`) {
		t.Errorf("an absent tableHealth is not [] in --json: %s", healthy)
	}
}

func TestQueryJQReadsTableHealth(t *testing.T) {
	out, _, err := runQueryCapturing(t, tableHealthEnvelope, "--jq", ".tableHealth[0].upstreamMore", "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if strings.TrimSpace(out) != "2" {
		t.Errorf("--jq .tableHealth[0].upstreamMore = %q", out)
	}
}
