package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The method may come first as a positional, the way an agent writes it after
// reading an endpoint list: `ronja api GET /api/v2/...`.
func TestAPITakesTheMethodAsAPositional(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"GET", "/api/v2/thing"}, http.MethodGet},
		{[]string{"delete", "/api/v2/thing"}, http.MethodDelete},
		{[]string{"Post", "/api/v2/thing", "-d", "{}"}, http.MethodPost},
		// The same method twice is one request, not a conflict.
		{[]string{"-X", "put", "PUT", "/api/v2/thing", "-d", "{}"}, http.MethodPut},
	} {
		e := newEchoServer(t)
		signInTo(t, e.URL())
		if _, err := runCLI(t, t.TempDir(), append([]string{"api"}, c.args...)...); err != nil {
			t.Fatalf("api %v: %v", c.args, err)
		}
		req := e.only()
		if req.Method != c.want || req.Path != "/api/v2/thing" {
			t.Errorf("api %v sent %s %s, want %s /api/v2/thing", c.args, req.Method, req.Path, c.want)
		}
	}
}

func TestAPIRefusesAPositionalMethodThatDisagreesWithX(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "-X", "POST", "GET", "/api/v2/thing")
	if err == nil {
		t.Fatal("-X POST with a positional GET was accepted")
	}
	if !strings.Contains(err.Error(), "POST") || !strings.Contains(err.Error(), "GET") {
		t.Errorf("the refusal does not name both methods: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

// Negative control: a first argument that is not a method keeps cobra's arity
// error, and the --jq flag-order guard still runs before it.
func TestAPIKeepsTheArityErrorForANonMethod(t *testing.T) {
	e := newEchoServer(t)
	signInTo(t, e.URL())

	_, err := runCLI(t, t.TempDir(), "api", "foo", "/x")
	if err == nil || !strings.Contains(err.Error(), "accepts 1 arg(s), received 2") {
		t.Errorf("api foo /x = %v, want the arity error", err)
	}
	_, err = runCLI(t, t.TempDir(), "api", "GET", "/x", "--jq", "-r", ".id")
	if err == nil || !strings.Contains(err.Error(), "--jq was given") {
		t.Errorf("api GET /x --jq -r .id = %v, want the flag-order refusal", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("a request went out anyway: %+v", e.requests)
	}
}

const appPageHTML = "<!doctype html><html><body>app</body></html>"

// A path outside the API answers with the web app's HTML page and a 200. That
// is not an answer an agent can use, so it is refused without printing it.
func TestAPIRefusesAWebAppPage(t *testing.T) {
	for _, c := range []struct{ path, named string }{
		{"/tables/table-abc", "/tables/table-abc"},
		{"/tables/table-abc?tab=rows", "/tables/table-abc"},
		// Cleaned as the server cleans it, so a dot segment cannot slip past.
		{"/api/../tables/table-abc", "/tables/table-abc"},
	} {
		e := newEchoServer(t)
		e.contentType = "text/html; charset=utf-8"
		e.body = appPageHTML
		signInTo(t, e.URL())

		for _, extra := range [][]string{nil, {"--jq", "."}} {
			args := append([]string{"api", c.path}, extra...)
			out, err := runCLI(t, t.TempDir(), args...)
			want := c.named + " is a page of the web app, not an API endpoint — API paths start with /api/v2/; find the one you need in /docs/api/endpoints.md"
			if err == nil || err.Error() != want {
				t.Errorf("api %v = %v, want %q", args, err, want)
			}
			if out != "" {
				t.Errorf("api %v printed the page: %q", args, out)
			}
		}
	}
}

// Negative controls: HTML under /docs/ is a real answer, JSON is printed under
// any path, and --out is an explicit request for the file.
func TestAPIPrintsHTMLWhereItIsTheAnswer(t *testing.T) {
	e := newEchoServer(t)
	e.contentType = "text/html"
	e.body = appPageHTML
	signInTo(t, e.URL())
	out, err := runCLI(t, t.TempDir(), "api", "/docs/api/endpoints.html")
	if err != nil || out != appPageHTML {
		t.Errorf("api /docs/... = %q, %v; want the page", out, err)
	}

	j := newEchoServer(t)
	signInTo(t, j.URL())
	out, err = runCLI(t, t.TempDir(), "api", "/tables/table-abc")
	if err != nil || out != `{"ok":true}` {
		t.Errorf("JSON under a non-API path = %q, %v; want it printed", out, err)
	}

	h := newEchoServer(t)
	h.contentType = "text/html"
	h.body = appPageHTML
	signInTo(t, h.URL())
	dir := t.TempDir()
	dest := filepath.Join(dir, "page.html")
	if _, err := runCLI(t, dir, "api", "/tables/table-abc", "--out", dest); err != nil {
		t.Fatalf("--out on a web-app page: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != appPageHTML {
		t.Errorf("--out saved %q, want the page", got)
	}
}

// --wait-until tests each polled answer, so a web-app page is refused there
// too, rather than failing as a jq parse error.
func TestAPIWaitUntilRefusesAWebAppPage(t *testing.T) {
	e := newEchoServer(t)
	e.contentType = "text/html; charset=utf-8"
	e.body = appPageHTML
	signInTo(t, e.URL())

	out, err := runCLI(t, t.TempDir(), "api", "/tables/table-abc", "--wait-until", ".ok", "--wait-timeout", "5")
	want := "/tables/table-abc is a page of the web app, not an API endpoint — API paths start with /api/v2/; find the one you need in /docs/api/endpoints.md"
	if err == nil || err.Error() != want {
		t.Errorf("api --wait-until on a web-app page = %v, want %q", err, want)
	}
	if out != "" {
		t.Errorf("api --wait-until printed the page: %q", out)
	}
}

// An HTML error page (a load balancer's 502 on a web path) is not the web app's
// page, so the refusal says what answered, and still where API paths live. The
// wait loop says the same before its own error branch.
func TestAPIRefusesAnHTMLErrorPageByItsStatus(t *testing.T) {
	e := newEchoServer(t)
	e.status = http.StatusBadGateway
	e.contentType = "text/html"
	e.body = "<html><body>502 Bad Gateway</body></html>"
	signInTo(t, e.URL())

	want := "HTTP 502 GET /tables/table-abc answered with an HTML page (not printed) — API paths start with /api/v2/; find the one you need in /docs/api/endpoints.md"
	for _, extra := range [][]string{nil, {"--wait-until", ".ok", "--wait-timeout", "5"}} {
		args := append([]string{"api", "/tables/table-abc"}, extra...)
		code, stderr, out := runQuiet(t, args...)
		if code != 1 || stderr != "error: "+want+"\n" || out != "" {
			t.Errorf("ronja %v: exit %d, stderr %q, stdout %q; want exit 1, %q and nothing printed", args, code, stderr, out, want)
		}
	}
}

// HEAD has no body to print, so a web path answered with HTML headers is not
// refused.
func TestAPIHeadOnAWebPathIsNotRefused(t *testing.T) {
	e := newEchoServer(t)
	e.contentType = "text/html"
	e.body = appPageHTML
	signInTo(t, e.URL())
	if _, err := runCLI(t, t.TempDir(), "api", "HEAD", "/tables/table-abc"); err != nil {
		t.Errorf("api HEAD on a web path = %v, want no refusal", err)
	}
}

// --out gets an HTTP error's body in the wait loop too, as it does without
// --wait-until, and nothing is printed to stdout.
func TestAPIWaitUntilSavesAnErrorBodyToOut(t *testing.T) {
	e := newEchoServer(t)
	e.status = http.StatusNotFound
	e.body = `{"error":"not found"}`
	signInTo(t, e.URL())
	dir := t.TempDir()
	dest := filepath.Join(dir, "answer.json")

	code, stderr, out := runQuiet(t, "api", "/api/v2/thing", "--wait-until", ".ok", "--out", dest)
	if got, _ := os.ReadFile(dest); string(got) != e.body {
		t.Errorf("--out saved %q, want the error body", got)
	}
	if code != 1 || out != "" || !strings.Contains(stderr, "HTTP 404 GET /api/v2/thing (while waiting)") {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, nothing on stdout, the status line", code, out, stderr)
	}
}
