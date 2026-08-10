package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// isolate points the credential store at a temp dir and clears the overrides,
// so tests never read or write the developer's real profile.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvConfig, dir)
	t.Setenv(EnvURL, "")
	t.Setenv(EnvToken, "")
	t.Setenv(EnvProfile, "")
	return dir
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain https", in: "https://app.ronja.tech", want: "https://app.ronja.tech"},
		{name: "trailing slash dropped", in: "https://app.ronja.tech/", want: "https://app.ronja.tech"},
		{name: "http preserved", in: "http://localhost:8080", want: "http://localhost:8080"},
		// A bare host is what people actually type.
		{name: "bare loopback assumes http", in: "localhost:8080", want: "http://localhost:8080"},
		{name: "bare 127.0.0.1 assumes http", in: "127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "bare remote assumes https", in: "app.ronja.tech", want: "https://app.ronja.tech"},
		{name: "whitespace trimmed", in: "  https://app.ronja.tech  ", want: "https://app.ronja.tech"},
		// Query and fragment are not part of an instance's identity; keeping
		// them would fragment the credential store across equivalent URLs.
		{name: "query dropped", in: "https://app.ronja.tech?x=1", want: "https://app.ronja.tech"},
		{name: "fragment dropped", in: "https://app.ronja.tech#top", want: "https://app.ronja.tech"},
		// An API URL pasted as the instance root would otherwise produce
		// doubled paths (".../api/api/v2/...") that 404 with no clue why.
		{name: "api suffix stripped", in: "https://app.ronja.tech/api", want: "https://app.ronja.tech"},
		{name: "api/v2 suffix stripped", in: "https://app.ronja.tech/api/v2", want: "https://app.ronja.tech"},
		{name: "api suffix with trailing slash", in: "https://app.ronja.tech/api/", want: "https://app.ronja.tech"},
		// A genuine subpath deployment must survive.
		{name: "subpath preserved", in: "https://corp.example/ronja", want: "https://corp.example/ronja"},
		{name: "subpath with api suffix", in: "https://corp.example/ronja/api", want: "https://corp.example/ronja"},
		// Hostnames are case-insensitive, so an uppercase spelling must not
		// become a second credential-store key.
		{name: "host lowercased", in: "https://APP.Ronja.Tech", want: "https://app.ronja.tech"},
		{name: "bare uppercase loopback", in: "LOCALHOST:8080", want: "http://localhost:8080"},
		// Paths genuinely are case-sensitive.
		{name: "path case preserved", in: "https://corp.example/Ronja", want: "https://corp.example/Ronja"},
		// The loopback sniff is an EXACT host match: "localhost.evil.com" is a
		// registerable domain, and must not be talked to in cleartext.
		{name: "loopback lookalike is https", in: "localhost.evil.com", want: "https://localhost.evil.com"},
		{name: "loopback lookalike with port", in: "localhost.evil.com:8080", want: "https://localhost.evil.com:8080"},
		{name: "127.0.0.1 lookalike is https", in: "127.0.0.1.evil.com", want: "https://127.0.0.1.evil.com"},
		{name: "bare loopback with path", in: "localhost/ronja", want: "http://localhost/ronja"},
		{name: "bare ipv6 loopback", in: "[::1]:8080", want: "http://[::1]:8080"},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "whitespace only rejected", in: "   ", wantErr: true},
		{name: "non-http scheme rejected", in: "ftp://app.ronja.tech", wantErr: true},
		{name: "scheme with no host rejected", in: "https://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeURL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeURL(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeURL(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// login stores one credential the way `ronja login` does — claim the
// identity and fill it in, inside a single Update. Tests go through this rather
// than writing the map directly so they exercise the real allocation path.
func login(t *testing.T, url, tenantID, tenantName, token, loggedIn string) string {
	t.Helper()
	normalized, err := NormalizeURL(url)
	if err != nil {
		t.Fatalf("NormalizeURL(%q): %v", url, err)
	}
	var name string
	err = Update(func(f *File) error {
		var p *Profile
		var created bool
		name, p, created = f.ClaimProfile(normalized, tenantID, "")
		p.Token, p.TenantName, p.LoggedIn = token, tenantName, loggedIn
		// Only a freshly created profile carries a provisional name. Renaming
		// on every login would rename the profile the user deliberately chose.
		if created && tenantName != "" {
			if better := f.AutoName(normalized, tenantName, name); better != name {
				if err := f.Rename(name, better); err != nil {
					return err
				}
				name = better
			}
		}
		f.Current = name
		return nil
	})
	if err != nil {
		t.Fatalf("login(%s): %v", url, err)
	}
	return name
}

func TestLoadMissingFileIsFirstRun(t *testing.T) {
	isolate(t)
	f, err := Load()
	if err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if f.Profiles == nil {
		t.Fatal("Profiles map must be initialised so callers can write into it")
	}
	if len(f.Profiles) != 0 {
		t.Fatalf("expected no profiles, got %d", len(f.Profiles))
	}
}

func TestSaveRoundTripAndPermissions(t *testing.T) {
	dir := isolate(t)
	login(t, "localhost:8080", "ten-1", "", "tok-1", "2026-01-01T00:00:00Z")

	// The file holds a bearer token; it must not be readable by other users.
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode = %o, want 600", FileName, perm)
	}

	resolved, err := Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The bare host must resolve to the same key it was stored under.
	if resolved.URL != "http://localhost:8080" {
		t.Errorf("URL = %q, want http://localhost:8080", resolved.URL)
	}
	if resolved.Token != "tok-1" {
		t.Errorf("Token = %q, want tok-1", resolved.Token)
	}
	if resolved.TenantID != "ten-1" {
		t.Errorf("TenantID = %q, want ten-1", resolved.TenantID)
	}
	if resolved.FromEnv {
		t.Error("FromEnv should be false for a stored token")
	}
}

func TestResolvePrecedence(t *testing.T) {
	isolate(t)
	login(t, "http://localhost:8080", "ten-dev", "", "stored", "2026-01-01T00:00:00Z")
	prod := login(t, "https://app.ronja.tech", "ten-prod", "Acme", "prod", "2026-02-01T00:00:00Z")

	// The most recent login becomes current.
	resolved, err := Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Profile != prod || resolved.Token != "prod" {
		t.Errorf("current = %q/%q, want %s/prod", resolved.Profile, resolved.Token, prod)
	}

	// RONJA_URL selects by URL...
	t.Setenv(EnvURL, "http://localhost:8080")
	resolved, err = Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Token != "stored" {
		t.Errorf("with RONJA_URL, token = %q, want stored", resolved.Token)
	}

	// ...and an explicit flag beats RONJA_URL.
	resolved, err = Resolve("https://app.ronja.tech", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Token != "prod" {
		t.Errorf("with --url, token = %q, want prod", resolved.Token)
	}

	// --profile selects by name, ignoring RONJA_URL entirely.
	resolved, err = Resolve("", prod)
	if err != nil {
		t.Fatalf("Resolve --profile: %v", err)
	}
	if resolved.Token != "prod" || resolved.URL != "https://app.ronja.tech" {
		t.Errorf("--profile %s resolved to %q/%q", prod, resolved.URL, resolved.Token)
	}

	// RONJA_TOKEN outranks anything on disk, so a CI job never inherits
	// whoever last logged in on the machine.
	t.Setenv(EnvToken, "from-env")
	resolved, err = Resolve("http://localhost:8080", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Token != "from-env" {
		t.Errorf("token = %q, want from-env", resolved.Token)
	}
	if !resolved.FromEnv {
		t.Error("FromEnv must be true when the token came from the environment")
	}
}

// An explicitly named URL is a FILTER, never a fallback.
//
// The failure this guards is a credential leak, not an inconvenience: falling
// through to the current profile sends a PRODUCTION token to whatever host the
// user just named on the command line.
//
// Negative control: the second half asserts a case the pre-profile, URL-keyed
// store would also have passed, and the FIRST half is the one that distinguishes
// them — a current profile that exists, holds a token, and is on a different
// URL. A test that only checked "no profiles at all" would pass against an
// implementation that falls back, and prove nothing.
func TestExplicitURLNeverFallsBackToCurrent(t *testing.T) {
	isolate(t)
	login(t, "https://app.ronja.tech", "ten-prod", "Acme", "prod-token", "2026-02-01T00:00:00Z")

	resolved, err := Resolve("https://staging.ronja.tech", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Token != "" {
		t.Errorf("token = %q for an instance with no profile — the production credential leaked to a host the user named explicitly", resolved.Token)
	}
	if resolved.Profile != "" {
		t.Errorf("profile = %q, want none selected", resolved.Profile)
	}
	if resolved.URL != "https://staging.ronja.tech" {
		t.Errorf("URL = %q, want the URL that was asked for", resolved.URL)
	}

	// Same via the environment variable, which takes the identical path.
	t.Setenv(EnvURL, "https://staging.ronja.tech")
	resolved, err = Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve via RONJA_URL: %v", err)
	}
	if resolved.Token != "" {
		t.Errorf("token = %q via RONJA_URL, want empty", resolved.Token)
	}
}

// Two organizations on ONE instance is the case profiles exist for. A URL alone
// cannot choose between them, and choosing anyway is the difference between
// reading test data and writing to a customer's organization.
func TestAmbiguousURLRefusesToPick(t *testing.T) {
	isolate(t)
	acme := login(t, "https://app.ronja.tech", "ten-acme", "Acme Retail", "acme-token", "2026-01-01T00:00:00Z")
	north := login(t, "https://app.ronja.tech", "ten-north", "Northwind AB", "north-token", "2026-02-01T00:00:00Z")

	// Both logins survived — neither evicted the other.
	f, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Profiles) != 2 {
		t.Fatalf("stored %d profiles, want 2 — a second organization on one instance evicted the first", len(f.Profiles))
	}

	// `north` is current (logged in last), so the URL resolves to it rather
	// than erroring: an existing choice is a real signal, not a coin flip.
	resolved, err := Resolve("https://app.ronja.tech", "")
	if err != nil {
		t.Fatalf("Resolve with current among candidates: %v", err)
	}
	if resolved.Profile != north {
		t.Errorf("profile = %q, want the current profile %q", resolved.Profile, north)
	}

	// With current pointing elsewhere, the URL is genuinely ambiguous.
	if err := Update(func(f *File) error { f.Current = ""; return nil }); err != nil {
		t.Fatalf("clear current: %v", err)
	}
	_, err = Resolve("https://app.ronja.tech", "")
	var ambiguous *AmbiguousURLError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Resolve = %v, want an AmbiguousURLError", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("candidates = %v, want both profiles named so the user can choose", ambiguous.Candidates)
	}
	// Sorted, or the message reads differently on every run.
	if ambiguous.Candidates[0] != acme || ambiguous.Candidates[1] != north {
		t.Errorf("candidates = %v, want them sorted", ambiguous.Candidates)
	}
}

// Naming both a profile and a conflicting URL is refused, rather than one half
// of the command silently winning.
func TestProfileAndURLConflictIsRefused(t *testing.T) {
	isolate(t)
	prod := login(t, "https://app.ronja.tech", "ten-prod", "Acme", "prod-token", "2026-01-01T00:00:00Z")

	_, err := Resolve("http://localhost:8080", prod)
	var mismatch *ProfileURLMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Resolve = %v, want a ProfileURLMismatchError", err)
	}

	// Agreeing is fine — people do pass both.
	if _, err := Resolve("https://app.ronja.tech", prod); err != nil {
		t.Errorf("Resolve with a matching --url and --profile: %v", err)
	}

	// An unknown name is an error, not a silent fallback to current.
	if _, err := Resolve("", "nope"); !errors.Is(err, ErrNoProfile) {
		t.Errorf("Resolve with an unknown profile = %v, want ErrNoProfile", err)
	}
}

// An environment token and a stored profile are independent. Adopting the
// profile's organization would make `wf push` match one organization's binding
// and send its workflow id under the other's token.
func TestEnvTokenNeverAdoptsProfileTenant(t *testing.T) {
	isolate(t)
	login(t, "https://app.ronja.tech", "ten-acme", "Acme", "acme-token", "2026-01-01T00:00:00Z")

	t.Setenv(EnvToken, "some-other-orgs-token")
	resolved, err := Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.TenantID != "" {
		t.Errorf("TenantID = %q under an environment token — the profile's organization says nothing about which one that token reaches", resolved.TenantID)
	}
	if !resolved.FromEnv || resolved.Token != "some-other-orgs-token" {
		t.Errorf("FromEnv=%v token=%q, want the environment credential", resolved.FromEnv, resolved.Token)
	}
	// The profile is still what chose the URL, and saying so is useful.
	if resolved.URL != "https://app.ronja.tech" {
		t.Errorf("URL = %q, want the profile's", resolved.URL)
	}
}

// On a fresh install, with nothing stored and nothing exported, the CLI points
// at the hosted instance.
//
// The literal is pinned rather than compared against the constant, which would
// be tautological and pass whatever the constant said. What this guards is a
// default quietly moving back to a local backend for someone's convenience:
// almost nobody running this has one, and pointing at it fails as "connection
// refused" — a message that says nothing about what to do instead.
func TestResolveDefaultsToTheHostedInstance(t *testing.T) {
	isolate(t)

	const hosted = "https://cloud.ronja.tech"
	if DefaultURL != hosted {
		t.Errorf("DefaultURL = %q, want the hosted instance %q", DefaultURL, hosted)
	}
	// And it must already be canonical, or the default disagrees with the key
	// every stored profile is written under.
	if normalized, err := NormalizeURL(DefaultURL); err != nil || normalized != DefaultURL {
		t.Errorf("NormalizeURL(DefaultURL) = %q, %v — the default is not in canonical form", normalized, err)
	}

	resolved, err := Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.URL != hosted {
		t.Errorf("URL = %q, want %q", resolved.URL, hosted)
	}
	if resolved.Token != "" {
		t.Errorf("Token = %q, want empty on a fresh install", resolved.Token)
	}
	// A default is not a login. Nothing is stored until someone authenticates.
	if resolved.Profile != "" {
		t.Errorf("Profile = %q, want none — the default is a destination, not a credential", resolved.Profile)
	}
}

func TestRemoveProfile(t *testing.T) {
	isolate(t)
	local := login(t, "http://localhost:8080", "ten-dev", "", "a", "2026-01-01T00:00:00Z")
	prod := login(t, "https://app.ronja.tech", "ten-prod", "Acme", "b", "2026-02-01T00:00:00Z")

	var removed bool
	if err := Update(func(f *File) error { removed = f.Remove(prod); return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !removed {
		t.Error("Remove should report that it removed a stored profile")
	}

	// Removing the current profile must fall back to a remaining one rather
	// than leaving the CLI with nothing selected.
	resolved, err := Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Profile != local || resolved.Token != "a" {
		t.Errorf("after removing current, resolved to %q/%q", resolved.Profile, resolved.Token)
	}

	// Removing something that was never stored is not an error, but must be
	// reported honestly so logout does not claim a success that did not happen.
	if err := Update(func(f *File) error { removed = f.Remove("nowhere"); return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if removed {
		t.Error("Remove reported a removal for a profile that was never stored")
	}
}

// With two or more profiles left, the promoted current must be the most recent
// login rather than whatever Go's randomised map iteration happened to yield —
// otherwise one logout can silently point the next command at production.
func TestRemovePromotesMostRecentLogin(t *testing.T) {
	isolate(t)
	login(t, "http://localhost:8080", "t1", "", "local", "2026-01-01T00:00:00Z")
	staging := login(t, "https://staging.ronja.tech", "t2", "Acme", "staging", "2026-03-01T00:00:00Z")
	prod := login(t, "https://app.ronja.tech", "t3", "Acme", "prod", "2026-02-01T00:00:00Z")

	// prod is current (written last), so removing it forces the fallback
	// choice among the two remaining.
	if err := Update(func(f *File) error { f.Remove(prod); return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Run it repeatedly: a nondeterministic implementation passes a single
	// check by luck roughly half the time.
	for i := 0; i < 20; i++ {
		resolved, err := Resolve("", "")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if resolved.Profile != staging {
			t.Fatalf("current = %q, want the most recently logged-in profile %q", resolved.Profile, staging)
		}
	}
}

// Ties (and profiles the user hand-edited to have no login stamp) still have to
// resolve to one answer, not a coin flip.
func TestPickCurrentBreaksTiesByName(t *testing.T) {
	f := &File{Profiles: map[string]*Profile{
		"b-org": {LoggedIn: "2026-01-01T00:00:00Z"},
		"a-org": {LoggedIn: "2026-01-01T00:00:00Z"},
		"c-org": {},
	}}
	for i := 0; i < 20; i++ {
		if got := f.pickCurrent(); got != "a-org" {
			t.Fatalf("pickCurrent = %q, want the sorted-first profile on a tie", got)
		}
	}
	if got := (&File{Profiles: map[string]*Profile{}}).pickCurrent(); got != "" {
		t.Errorf("pickCurrent on an empty store = %q, want empty", got)
	}
}

// Concurrent logins must all survive — including the hard case: several
// organizations on ONE instance, which all derive the SAME provisional name.
//
// Two separate failures are covered. Unlocked, the later Save writes a snapshot
// that predates the earlier one and drops a token outright. Correctly locked but
// with name allocation OUTSIDE the lock, every racer picks the same free name
// and the last writer overwrites the rest — the same lost credential, one layer
// up, and the exact bug profiles exist to fix.
func TestConcurrentLoginsKeepEveryCredential(t *testing.T) {
	isolate(t)

	const url = "https://app.ronja.tech"
	tenants := []string{"ten-a", "ten-b", "ten-c", "ten-d"}
	// A second instance in the mix, so the test covers both axes at once.
	other := "https://staging.ronja.tech"

	errs := make(chan error, len(tenants)+1)
	start := make(chan struct{})
	claim := func(instance, tenant string) {
		<-start
		errs <- Update(func(f *File) error {
			name, p, _ := f.ClaimProfile(instance, tenant, "")
			p.Token = "tok-" + tenant
			p.LoggedIn = "2026-01-01T00:00:00Z"
			f.Current = name
			return nil
		})
	}
	for _, tenant := range tenants {
		go claim(url, tenant)
	}
	go claim(other, "ten-staging")
	close(start)
	for range len(tenants) + 1 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent login: %v", err)
		}
	}

	f, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Profiles) != len(tenants)+1 {
		t.Fatalf("stored %d profiles, want %d — a concurrent login overwrote another organization's token",
			len(f.Profiles), len(tenants)+1)
	}
	for _, tenant := range tenants {
		name, p := f.FindIdentity(url, tenant)
		if p == nil {
			t.Errorf("credential for %s on %s was lost by a concurrent write", tenant, url)
			continue
		}
		if p.Token != "tok-"+tenant {
			t.Errorf("profile %q holds token %q, want %q", name, p.Token, "tok-"+tenant)
		}
	}
	if _, p := f.FindIdentity(other, "ten-staging"); p == nil {
		t.Errorf("credential for the second instance was lost")
	}
}

// Re-authenticating updates the profile you already have. It must not mint a
// second one beside it, and must not rename the one scripts refer to.
func TestReLoginKeepsProfileNameAndIdentity(t *testing.T) {
	isolate(t)
	first := login(t, "https://app.ronja.tech", "ten-acme", "Acme Retail", "tok-1", "2026-01-01T00:00:00Z")
	if err := Update(func(f *File) error { return f.Rename(first, "work") }); err != nil {
		t.Fatalf("rename: %v", err)
	}

	again := login(t, "https://app.ronja.tech", "ten-acme", "Acme Retail", "tok-2", "2026-02-01T00:00:00Z")
	if again != "work" {
		t.Errorf("re-login landed in %q, want the renamed profile %q", again, "work")
	}
	f, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Profiles) != 1 {
		t.Fatalf("re-login created %d profiles, want 1", len(f.Profiles))
	}
	if f.Profiles["work"].Token != "tok-2" {
		t.Errorf("token = %q, want the refreshed tok-2", f.Profiles["work"].Token)
	}
}

// A --profile name held by a DIFFERENT organization must not be taken. The new
// credential still has to land somewhere: the server has already minted it and
// shown it to nobody else, so discarding it strands a live token.
func TestClaimProfileNeverOverwritesAnotherIdentity(t *testing.T) {
	isolate(t)
	login(t, "https://app.ronja.tech", "ten-acme", "Acme Retail", "acme-token", "2026-01-01T00:00:00Z")

	var landed string
	if err := Update(func(f *File) error {
		var p *Profile
		landed, p, _ = f.ClaimProfile("https://app.ronja.tech", "ten-north", "acme-retail")
		p.Token = "north-token"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if landed == "acme-retail" {
		t.Fatal("the requested name was taken from the organization already holding it")
	}

	f, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Profiles["acme-retail"].Token; got != "acme-token" {
		t.Errorf("incumbent token = %q, want it untouched", got)
	}
	if got := f.Profiles[landed].Token; got != "north-token" {
		t.Errorf("new credential = %q, want it stored under %q rather than discarded", got, landed)
	}
}

func TestLoadRejectsCorruptFileClearly(t *testing.T) {
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load should surface a corrupt credential file rather than silently resetting it")
	}
}

// A hostname whose label happens to end in "api" must not be mangled — the
// suffix strip is a PATH rule, not a substring rule.
func TestNormalizeURLDoesNotEatHostnames(t *testing.T) {
	for _, in := range []string{"https://api.ronja.tech", "https://myapi.example"} {
		got, err := NormalizeURL(in)
		if err != nil {
			t.Fatalf("NormalizeURL(%q): %v", in, err)
		}
		if got != in {
			t.Errorf("NormalizeURL(%q) = %q, want it unchanged", in, got)
		}
	}
}

// A STALE lock must admit exactly one writer, not everybody who saw it.
//
// This reproduces the two-writer ordering EXACTLY rather than hoping the
// scheduler produces it: both waiters observe the same stale content, the first
// evicts it and re-acquires, and only then does the second act on its stale
// observation. With an unconditional Stat-then-Remove takeover the second
// waiter's Remove deletes the FIRST waiter's brand-new lock, after which both
// O_EXCL creates succeed and both run the critical section — which is a
// read-modify-write of the whole credential file, so "both ran" means one
// login's token is silently dropped: it exists on the server, was displayed
// nowhere, and cannot be recovered.
func TestStaleTakeoverAdmitsOneEvictor(t *testing.T) {
	dir := isolate(t)
	lockPath := filepath.Join(dir, FileName+".lock")
	stale := lockBody("deadbeefdeadbeefdeadbeef")
	if err := os.WriteFile(lockPath, []byte(stale), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	// Waiter A and waiter B both read the stale lock and judge it abandoned.
	// (Both observations are the same string; that is the premise.)

	// A evicts and acquires.
	if !evictObservedLock(lockPath, stale) {
		t.Fatal("the first evictor should have taken over the abandoned lock")
	}
	fresh := lockBody("11112222333344445555666")
	acquired, err := tryAcquireLock(lockPath, fresh)
	if err != nil || !acquired {
		t.Fatalf("first evictor could not acquire after eviction: acquired=%v err=%v", acquired, err)
	}

	// B now acts on its (now obsolete) observation. It must NOT remove A's lock.
	if evictObservedLock(lockPath, stale) {
		t.Error("a second evictor took over a lock that had already been re-acquired — two writers")
	}
	current, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("the live holder's lock was deleted by a stale observation: %v", err)
	}
	if string(current) != fresh {
		t.Errorf("lock body = %q, want the live holder's %q", current, fresh)
	}
}

// The end-to-end shape: concurrent withLock callers racing a pre-existing stale
// lock must serialize. Complements the deterministic test above by exercising
// the real acquire/evict/release loop.
func TestWithLockStaleTakeoverSerializes(t *testing.T) {
	dir := isolate(t)
	lockPath := filepath.Join(dir, FileName+".lock")
	if err := os.WriteFile(lockPath, []byte(lockBody("deadbeefdeadbeefdeadbeef")), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}
	// Older than the staleness threshold by a wide margin, so every racer
	// certainly sees it as abandoned.
	old := time.Now().Add(-10 * lockStale)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatalf("age the lock: %v", err)
	}

	const racers = 4
	var inside atomic.Int32
	var overlapped atomic.Bool
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, racers)

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = withLock(func() error {
				if inside.Add(1) > 1 {
					overlapped.Store(true)
				}
				// Long enough that a genuine overlap cannot be missed by
				// scheduling luck, short enough that every racer still fits
				// inside lockWait rather than timing out.
				time.Sleep(50 * time.Millisecond)
				inside.Add(-1)
				return nil
			})
		}()
	}
	close(start)
	wg.Wait()

	if overlapped.Load() {
		t.Error("two writers held the credential lock at once")
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: %v", i, err)
		}
	}
	// And nothing is left behind for the next command to trip over — neither
	// the lock nor an eviction claim.
	if _, err := os.Stat(lockPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lock file survived every holder: stat err = %v", err)
	}
	leftovers, _ := filepath.Glob(lockPath + ".takeover.*")
	if len(leftovers) > 0 {
		t.Errorf("eviction claims left behind: %v", leftovers)
	}
}

// A process that has been taken over must NOT delete its successor's lock on
// the way out. The unconditional `defer os.Remove(lockPath)` this replaces did
// exactly that, admitting a third writer behind the second.
func TestReleaseLockLeavesAnotherHoldersLock(t *testing.T) {
	dir := isolate(t)
	lockPath := filepath.Join(dir, FileName+".lock")
	successor := lockBody("aaaabbbbccccdddd11112222")
	if err := os.WriteFile(lockPath, []byte(successor), 0o600); err != nil {
		t.Fatalf("seed successor lock: %v", err)
	}

	// The evicted process releases with ITS OWN body, which no longer matches.
	releaseLock(lockPath, lockBody("ffff0000ffff0000ffff0000"))

	current, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("successor's lock was deleted by a process that no longer held it: %v", err)
	}
	if string(current) != successor {
		t.Errorf("lock body = %q, want %q", current, successor)
	}

	// And the real holder can still release it.
	releaseLock(lockPath, successor)
	if _, err := os.Stat(lockPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("holder could not release its own lock: stat err = %v", err)
	}
}

// The wait bound and the staleness threshold are separate knobs on purpose: a
// holder whose read-modify-write outlasts the wait (a network home directory, a
// laptop resuming from sleep) must not be evicted while it is still running.
func TestLockStaleIsWellClearOfLockWait(t *testing.T) {
	if lockStale <= 10*lockWait {
		t.Errorf("lockStale (%s) must be an order of magnitude beyond lockWait (%s), else a slow holder is evicted mid-write",
			lockStale, lockWait)
	}
}

func TestEvictionKey(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"nonce extracted", "pid=123 nonce=abcdef012345\n", "abcdef012345"},
		{"legacy empty lock", "", "unidentified"},
		{"truncated write", "pid=123 non", "unidentified"},
		// Never let attacker-ish content become a path traversal in the claim
		// filename; anything that is not plain hex collapses to the shared key.
		{"non-hex rejected", "nonce=../../etc/passwd", "unidentified"},
		{"uppercase rejected", "nonce=ABCDEF", "unidentified"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evictionKey(tt.body); got != tt.want {
				t.Errorf("evictionKey(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}
