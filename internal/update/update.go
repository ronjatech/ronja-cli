// Package update owns the CLI's knowledge of its own releases: which one is
// current, which one is newest, how a release archive is named, and how the
// binary on disk was installed.
//
// Two constraints shape the whole package.
//
//  1. It never carries the Ronja credential. internal/api attaches
//     `Authorization: Bearer <token>` to everything it sends, so a release
//     fetch routed through it would hand a Ronja access token to github.com on
//     every check. This package therefore does NOT import internal/api — a test
//     pins that import boundary, because the invariant is structural and a
//     future convenience refactor is exactly how it would be lost.
//  2. The release source is a compiled-in constant. There is no env var that
//     redirects it, so nothing in the environment can decide what bytes
//     `ronja update` downloads and writes over the running binary. Tests reach
//     it through one package var in internal/commands and nowhere else.
//
// Everything here is stdlib-only apart from internal/config, which owns the
// directory the state file lives in.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the GitHub API host the release lookup runs against.
	DefaultBaseURL = "https://api.github.com"

	// Repo is the public mirror the CLI is published to. The monorepo builds
	// the release; the mirror is where it lands and where `go install` and the
	// Homebrew cask both point.
	Repo = "ronjatech/ronja-cli"

	// ChecksumsName is the manifest goreleaser publishes beside the archives
	// (`checksum.name_template: checksums.txt` in .goreleaser.yaml).
	ChecksumsName = "checksums.txt"

	// maxReleaseBody caps the release JSON. A release with four archives and a
	// checksum file is ~12 KB; 1 MiB is room for a decade of growth and still a
	// ceiling, so a wrong or hostile endpoint cannot stream forever.
	maxReleaseBody = 1 << 20
)

// Release is the one release the CLI cares about: its tag and the download URL
// of every asset attached to it, keyed by asset name.
type Release struct {
	Version string
	Assets  map[string]string
}

// ReleasesPage is where a human goes when the automated path refuses.
func ReleasesPage() string {
	return "https://github.com/" + Repo + "/releases"
}

// ReleasePage is the page for one specific release.
func ReleasePage(version string) string {
	return ReleasesPage() + "/tag/" + version
}

// Latest reports the newest published release on the mirror.
//
// base is the API host (DefaultBaseURL in production; a fake in tests) and
// version is the running CLI's version, which travels in the User-Agent so a
// rate-limited or misbehaving client is identifiable from GitHub's side.
//
// Worth knowing, and documented in cli/README.md rather than engineered around:
// GitHub's `releases/latest` is the most recently CREATED non-prerelease, not
// the highest semantic version. A re-cut of an older line published after a
// newer one would be answered here. That matches what `go install ...@latest`
// and the Homebrew cask do, so all three install paths agree.
func Latest(ctx context.Context, base, version string) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", base, Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, fmt.Errorf("build release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ronja-cli/"+version)
	// Deliberately no Authorization header. See the package doc.

	resp, err := releaseHTTPClient().Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	// One byte past the cap, so "at the cap" and "over it" are distinguishable.
	// Reading exactly maxReleaseBody and handing the truncated bytes to the
	// parser reports a JSON syntax error, which names the wrong problem: the
	// answer was not malformed, it was too big to read.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseBody+1))
	if err != nil {
		return Release{}, fmt.Errorf("read release: %w", err)
	}
	if len(body) > maxReleaseBody {
		return Release{}, fmt.Errorf("the release description from %s is larger than %d KiB", url, maxReleaseBody>>10)
	}
	var payload struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Release{}, fmt.Errorf("parse release: %w", err)
	}
	if payload.TagName == "" {
		return Release{}, fmt.Errorf("release from %s carries no tag_name", url)
	}
	rel := Release{Version: payload.TagName, Assets: map[string]string{}}
	for _, a := range payload.Assets {
		if a.Name != "" && a.URL != "" {
			rel.Assets[a.Name] = a.URL
		}
	}
	return rel, nil
}

// responseHeaderTimeout bounds how long a server may take to answer at all —
// to send status and headers — once the request is on the wire. It is not a
// bound on the body; see idleReader for the one that is.
const responseHeaderTimeout = 30 * time.Second

// releaseHTTPClient is built by CLONING http.DefaultTransport, so it keeps
// ProxyFromEnvironment and therefore honours HTTPS_PROXY / NO_PROXY. A
// hand-built transport was the obvious alternative and is wrong: on a corporate
// machine where github.com is only reachable through a proxy it would fail
// every check, silently.
//
// There is no Client.Timeout, deliberately. Client.Timeout covers the whole
// exchange including the body, and for the multi-megabyte archive that is a
// bandwidth requirement wearing a clock's clothes. The bounds that do apply:
//
//   - the caller's context — 15 s around the release lookup in `ronja update`,
//     1 s around the daily check; the archive download is deliberately left to
//     the command's own context, which is cancelled by Ctrl-C and nothing else;
//   - ResponseHeaderTimeout, on every request this client makes, for a server
//     that accepts a connection and never answers;
//   - the idle-read watchdog in download.go, for a body that starts and stalls.
//
// Built once (the clone carries its own connection pool, and a fresh one per
// request would discard every keep-alive connection between the checksum
// manifest and the archive that follows it).
var releaseHTTPClient = sync.OnceValue(func() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Only reachable if something replaced http.DefaultTransport. A plain
		// client on whatever that is beats refusing to check for updates.
		return &http.Client{CheckRedirect: guardRedirect}
	}
	tr := transport.Clone()
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: tr, CheckRedirect: guardRedirect}
})

// maxRedirects is net/http's own default, restated because installing a
// CheckRedirect replaces the default entirely — including the hop limit that
// stops a redirect loop.
const maxRedirects = 10

// guardRedirect stops a redirect that would leave the release's transport or
// its publisher behind.
//
// requireScheme and requireKnownHost refuse a bad URL before the request is
// made, but a server can answer with a 302 to anywhere and net/http follows it
// by default — so both rules have to hold across the hops too, or they hold
// only until somebody redirects. That matters most on the archive: these bytes
// are about to be written over the running executable, so on a plaintext hop
// anything between here and the mirror gets to choose them, and on a hop to
// somebody else's host that somebody chooses them outright.
//
// The origin of the chain stands in for the release source here, because
// CheckRedirect is not told what base named the URL — and it is the right
// stand-in: the first request's own host was checked against the base before
// it went out.
func guardRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	origin := via[0].URL
	if origin.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect from %s to %s: %s would be read over a weaker transport than it was named on",
			origin.Scheme, req.URL.Scheme, Safe(req.URL.Redacted()))
	}
	if !releaseHostAllowed(req.URL, origin) {
		return fmt.Errorf("refusing a redirect to %s: %s is not a host ronja releases are published on",
			Safe(req.URL.Redacted()), Safe(req.URL.Host))
	}
	return nil
}

// requireKnownHost refuses to fetch a URL from a host the CLI's releases are
// not published on.
//
// The URLs this package fetches come out of the release listing, which means
// whoever can publish or tamper with that listing chooses them — and the
// archive's bytes are about to be written over the running executable. https
// alone does not help there: a valid certificate for a host of the attacker's
// own choosing is free. So the host is pinned as well.
//
// This is narrowing, not proof: github.com serving the archive is the same
// origin that serves the manifest it is checked against, which is exactly the
// hole a signature would close. See BL-6d3a.
func requireKnownHost(base, target string) error {
	b, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("parse the release source %q: %w", base, err)
	}
	t, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("parse %q: %w", target, err)
	}
	if releaseHostAllowed(t, b) {
		return nil
	}
	return fmt.Errorf("refusing to fetch %s: %s is not a host ronja releases are published on",
		Safe(t.Redacted()), Safe(t.Host))
}

// releaseHostAllowed reports whether u may be fetched, given the release source
// that named it.
//
// The base's own host is always allowed, and that is what lets the same rule
// run under test: in production the base is the compiled-in api.github.com
// constant, so allowing it adds nothing, while the tests point it at an
// httptest server on loopback and the fake mirror serves every asset itself.
// The alternative — a flag that switches the rule off for tests — would mean
// production carries the switch too.
func releaseHostAllowed(u, base *url.URL) bool {
	if base != nil && u.Host == base.Host {
		return true
	}
	host := strings.ToLower(u.Hostname())
	if host == "github.com" || host == "api.github.com" {
		return true
	}
	// Release assets are served from a SUBDOMAIN of githubusercontent.com, and
	// which subdomain is GitHub's to change — objects. and release-assets. have
	// each been it. The rule is therefore the domain rather than one exact
	// host: pinning the subdomain would refuse every download the day GitHub
	// moves it, and "ronja update stopped working" is a worse failure than the
	// narrowing this loses (the domain is GitHub's, not a third party's).
	return host == "githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

// Safe renders a string the release listing chose for a terminal.
//
// A URL out of the release listing was written by whoever published it, and
// stderr is a terminal that honours control sequences: a URL carrying an ANSI
// escape or a right-to-left override can redraw the line it is printed on, and
// every line these URLs appear on is a refusal — one that can be made to read
// as a success is worse than no message at all.
//
// This is the second half of a rule the version parser already carries: a TAG
// is admitted to a printed line only by passing validPrerelease, which is a
// charset test for exactly this reason, so tags and the asset names built from
// them are printed verbatim. URLs have no such gate, which is why they have
// this one.
//
// strconv.Quote is the whole implementation because it escapes exactly the
// runes at issue — Go's unicode.IsPrint excludes the Cf category, so bidi
// overrides and zero-width characters are escaped alongside the C0 controls —
// and leaves an ordinary URL readable.
func Safe(s string) string {
	return strconv.Quote(s)
}

// requireScheme refuses to fetch a URL over a weaker transport than the release
// listing that named it.
//
// The rule is comparative rather than a bare `https://` test, and that shape is
// the point. In production base is the compiled-in https constant, so this IS
// "https only": a release listing that named a plaintext asset URL — because it
// was tampered with, or because somebody mis-templated it — is refused before a
// byte is fetched. The tests point base at an httptest server, which speaks
// http, and comparing against the base is what lets the same guard run there
// rather than being switched off by a flag production would also carry. An
// https URL is accepted from either base: nothing is ever downgraded by it.
func requireScheme(base, target string) error {
	b, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("parse the release source %q: %w", base, err)
	}
	t, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("parse %q: %w", target, err)
	}
	if t.Scheme == "https" || t.Scheme == b.Scheme {
		return nil
	}
	return fmt.Errorf("refusing to fetch %s over %q: the release was read over %q",
		Safe(t.Redacted()), t.Scheme, b.Scheme)
}
