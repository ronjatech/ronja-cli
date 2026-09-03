package update

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const sampleRelease = `{
  "tag_name": "v0.29.0",
  "assets": [
    {"name": "ronja_0.29.0_darwin_arm64.tar.gz", "browser_download_url": "https://example.invalid/a.tar.gz"},
    {"name": "checksums.txt", "browser_download_url": "https://example.invalid/checksums.txt"}
  ]
}`

func TestLatestParsesTagAndAssets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+Repo+"/releases/latest" {
			t.Errorf("requested %s, want the latest-release path", r.URL.Path)
		}
		w.Write([]byte(sampleRelease))
	}))
	defer srv.Close()

	rel, err := Latest(context.Background(), srv.URL, "v0.28.4")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Version != "v0.29.0" {
		t.Errorf("version = %q, want v0.29.0", rel.Version)
	}
	if rel.Assets["checksums.txt"] != "https://example.invalid/checksums.txt" {
		t.Errorf("checksums asset = %q", rel.Assets["checksums.txt"])
	}
	if len(rel.Assets) != 2 {
		t.Errorf("assets = %v, want 2 entries", rel.Assets)
	}
}

// The invariant this package exists to protect: a release lookup must never
// carry the Ronja credential, so it carries no Authorization header at all.
func TestLatestSendsNoCredential(t *testing.T) {
	var auth, agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		agent = r.Header.Get("User-Agent")
		w.Write([]byte(sampleRelease))
	}))
	defer srv.Close()

	if _, err := Latest(context.Background(), srv.URL, "v0.28.4"); err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if auth != "" {
		t.Errorf("release request carried Authorization: %q", auth)
	}
	if !strings.HasPrefix(agent, "ronja-cli/") {
		t.Errorf("User-Agent = %q, want a ronja-cli/<version> agent", agent)
	}
}

func TestLatestErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"not found", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }},
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }},
		{"garbage body", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>nope")) }},
		{"no tag", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"assets":[]}`)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if _, err := Latest(context.Background(), srv.URL, "v0.28.4"); err == nil {
				t.Fatal("Latest succeeded, want an error")
			}
		})
	}
}

// A slow or black-holed mirror must end the lookup on the caller's deadline
// rather than hold the command.
func TestLatestRespectsDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Latest(ctx, srv.URL, "v0.28.4"); err == nil {
		t.Fatal("Latest succeeded against a sleeping handler, want a deadline error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Latest took %s, want it bounded by the context deadline", elapsed)
	}
}

// A release description bigger than the cap is reported as being too big, not
// as unparseable: reading exactly the cap and handing the truncated bytes to
// the JSON parser blames the answer's shape for its size.
func TestLatestRefusesAnOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Valid JSON throughout, so nothing but the size can fail it.
		w.Write([]byte(`{"tag_name":"v0.29.0","note":"`))
		w.Write(bytes.Repeat([]byte("x"), maxReleaseBody))
		w.Write([]byte(`"}`))
	}))
	defer srv.Close()

	_, err := Latest(context.Background(), srv.URL, "v0.28.4")
	if err == nil {
		t.Fatal("Latest accepted a body past the cap")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %q, want it to say the description is larger than the cap", err)
	}
	if strings.Contains(err.Error(), "parse release") {
		t.Errorf("error = %q — the answer was not malformed, it was too big", err)
	}
}

// The asset URL comes out of the release listing, and these bytes are about to
// be written over the running executable — so a plaintext URL named by an https
// listing is refused before the connection, not judged after it.
func TestFetchRefusesAWeakerSchemeThanTheBase(t *testing.T) {
	var asked bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = true
		w.Write([]byte("nothing anybody should have read\n"))
	}))
	defer srv.Close()

	// The production base, against the http URL a tampered listing would name.
	if _, err := Fetch(context.Background(), DefaultBaseURL, srv.URL, MaxChecksums); err == nil {
		t.Fatal("Fetch accepted an http asset URL from an https release")
	} else if !strings.Contains(err.Error(), "http") {
		t.Errorf("error = %q, want it to name the scheme", err)
	}
	if asked {
		t.Error("the refused URL was fetched anyway")
	}

	f, _, err := Download(context.Background(), DefaultBaseURL, srv.URL, MaxAsset)
	if err == nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatal("Download accepted an http asset URL from an https release")
	}

	// The same guard runs in the tests, where the base is the fake mirror: an
	// http URL is fine there because that is what the release was read over.
	if _, err := Fetch(context.Background(), srv.URL, srv.URL, MaxChecksums); err != nil {
		t.Errorf("Fetch from an http base: %v, want the same-scheme URL accepted", err)
	}
}

// Both rules have to survive the hops, or they hold only until somebody
// redirects: a server can answer an https request with a 302 to anywhere, and
// net/http follows it by default.
func TestGuardRedirect(t *testing.T) {
	hop := func(from, to string) (*http.Request, []*http.Request) {
		origin, _ := http.NewRequest(http.MethodGet, from, nil)
		next, _ := http.NewRequest(http.MethodGet, to, nil)
		return next, []*http.Request{origin}
	}
	cases := []struct {
		name     string
		from, to string
		refuse   bool
	}{
		{"https downgraded to http", "https://github.com/a", "http://github.com/a", true},
		{"https to another host of GitHub's", "https://github.com/a", "https://objects.githubusercontent.com/a", false},
		{"http to http, which is the test mirror", "http://127.0.0.1:1/a", "http://127.0.0.1:1/b", false},
		{"http upgraded to https", "http://github.com/a", "https://github.com/a", false},
		// The hop the host rule exists for: a tampered release listing cannot
		// name a foreign host directly (requireKnownHost), so it would name
		// github.com and have that answer a 302 to its own.
		{"https to somebody else's host", "https://github.com/a", "https://elsewhere.invalid/a", true},
		{"a redirect off the test mirror", "http://127.0.0.1:1/a", "http://127.0.0.2:1/a", true},
		// A subdomain of the release-asset domain is allowed whichever one
		// GitHub is currently using; a lookalike registered elsewhere is not.
		{"a newer asset subdomain", "https://github.com/a", "https://release-assets.githubusercontent.com/a", false},
		{"a lookalike domain", "https://github.com/a", "https://githubusercontent.com.evil.invalid/a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, via := hop(tc.from, tc.to)
			err := guardRedirect(req, via)
			if tc.refuse && err == nil {
				t.Error("the redirect was allowed")
			}
			if !tc.refuse && err != nil {
				t.Errorf("the redirect was refused: %v", err)
			}
		})
	}

	// Installing a CheckRedirect replaces net/http's default entirely, hop
	// limit included, so the loop guard has to be restated here.
	req, _ := http.NewRequest(http.MethodGet, "https://github.com/a", nil)
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = req
	}
	if err := guardRedirect(req, via); err == nil {
		t.Error("a redirect chain past the hop limit was allowed")
	}
}

// The asset URL comes out of the release listing, so a listing that has been
// tampered with can name any host it likes — and https does not help, because
// a certificate for a host of the attacker's own choosing is free. The host is
// therefore pinned before the connection.
func TestFetchRefusesAForeignHost(t *testing.T) {
	var asked bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = true
		w.Write([]byte("nothing anybody should have read\n"))
	}))
	defer srv.Close()

	// The production base, against an https URL on a host that is not GitHub's.
	// A different base than the target, which is what a real tampered listing
	// looks like: the release was read from api.github.com and points elsewhere.
	_, err := Fetch(context.Background(), DefaultBaseURL, "https://elsewhere.invalid/checksums.txt", MaxChecksums)
	if err == nil {
		t.Fatal("Fetch accepted an asset URL on a foreign host")
	}
	if !strings.Contains(err.Error(), "elsewhere.invalid") {
		t.Errorf("error = %q, want it to name the host", err)
	}

	f, _, downloadErr := Download(context.Background(), DefaultBaseURL, "https://elsewhere.invalid/a.tar.gz", MaxAsset)
	if downloadErr == nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatal("Download accepted an asset URL on a foreign host")
	}
	if asked {
		t.Error("the refused URL was fetched anyway")
	}

	// The hosts a real release names are allowed, and so is the base's own —
	// which is what lets the same rule run against the fake mirror rather than
	// being switched off under test.
	for _, ok := range []string{
		"https://github.com/ronjatech/ronja-cli/releases/download/v1/a.tar.gz",
		"https://objects.githubusercontent.com/a.tar.gz",
	} {
		if err := requireKnownHost(DefaultBaseURL, ok); err != nil {
			t.Errorf("requireKnownHost(%s) = %v, want it allowed", ok, err)
		}
	}
	if err := requireKnownHost(srv.URL, srv.URL+"/download/a.tar.gz"); err != nil {
		t.Errorf("requireKnownHost against the base's own host = %v, want it allowed", err)
	}
}

// A redirect that keeps the scheme and stays on the host is ordinary — the
// mirror answers one for every asset — and must still be followed.
func TestFetchFollowsASameSchemeRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			w.Write([]byte("the manifest\n"))
			return
		}
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.URL, srv.URL+"/checksums.txt", MaxChecksums)
	if err != nil {
		t.Fatalf("Fetch through a redirect: %v", err)
	}
	if string(got) != "the manifest\n" {
		t.Errorf("body = %q, want the redirected content", got)
	}
}
