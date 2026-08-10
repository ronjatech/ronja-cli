package config

import (
	"errors"
	"testing"
)

func TestSlug(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "Acme Retail", "acme-retail"},
		{"already a slug", "acme-retail", "acme-retail"},
		{"runs collapsed", "Acme  --  Retail", "acme-retail"},
		{"underscores and dots fold", "acme_retail.ab", "acme-retail-ab"},
		{"edges trimmed", "  -Acme-  ", "acme"},
		{"punctuation dropped", "Acme, Inc. (EU)", "acme-inc-eu"},
		// Anything outside the allowed set is dropped rather than
		// transliterated. The result is a poor name, but a usable one, and
		// `profile rename` exists.
		{"non-ascii dropped", "Ässä Oy", "ss-oy"},
		{"entirely non-ascii", "日本", ""},
		{"empty", "", ""},
		{"separators only", " _-. ", ""},
		// Long names are capped, and the cap must not leave a trailing hyphen.
		{"capped", "The Very Long Name Of Some Nordic Holding Company AB", "the-very-long-name-of-some-nordi"},
		{"cap lands on a separator", "abcdefghij klmnopqrst uvwxyzabcd efgh", "abcdefghij-klmnopqrst-uvwxyzabcd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Slug(tt.in)
			if got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got != "" {
				if err := ValidateName(got); err != nil {
					t.Errorf("Slug(%q) = %q, which is not a valid name: %v", tt.in, got, err)
				}
			}
		})
	}
}

func TestHostLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://staging.ronja.tech", "staging"},
		{"https://app.ronja.tech", "app"},
		// Several local backends at once is the normal way to work on Ronja —
		// one per worktree — and the port is the only thing telling them apart.
		{"http://localhost:8080", "local-8080"},
		{"http://localhost:8082", "local-8082"},
		{"http://127.0.0.1:8081", "local-8081"},
		{"http://[::1]:8083", "local-8083"},
		// No port to distinguish, so nothing to add.
		{"http://localhost", "local"},
		// A default port is the same instance as an omitted one, and says
		// nothing worth carrying into a name.
		{"https://app.ronja.tech:443", "app"},
		{"http://localhost:80", "local"},
		// A non-default port on a remote host is just as distinguishing.
		{"https://corp.example:8443", "corp-8443"},
		{"https://corp.example/ronja", "corp"},
		// A bare single-label host has no subdomain to take.
		{"https://intranet", "intranet"},
		{"https://10.0.0.7", "10-0-0-7"},
	}
	for _, tt := range tests {
		if got := HostLabel(tt.in); got != tt.want {
			t.Errorf("HostLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidateName(t *testing.T) {
	valid := []string{"acme", "acme-retail", "a", "app-2", "ten9"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
	// Typed names are validated rather than silently slugged: quietly turning
	// "Acme Prod" into "acme-prod" means the name the user asked for is not the
	// one any later command accepts.
	invalid := []string{"", "Acme", "acme retail", "acme_retail", "-acme", "acme-", "acme/prod", "ünïcode"}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
		}
	}
}

func TestAutoName(t *testing.T) {
	const prod = "https://app.ronja.tech"
	const staging = "https://staging.ronja.tech"
	const local = "http://localhost:8080"

	t.Run("organization names the profile", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{}}
		if got := f.AutoName(prod, "Acme Retail", ""); got != "acme-retail" {
			t.Errorf("AutoName = %q, want acme-retail", got)
		}
	})

	// A dev's local backend and production frequently carry the same
	// organization name, and "local" is what people actually call it.
	t.Run("loopback prefers the host label", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{}}
		if got := f.AutoName(local, "Development", ""); got != "local-8080" {
			t.Errorf("AutoName = %q, want local-8080", got)
		}
	})

	// The case this exists for: several local backends, one per worktree, all
	// reporting the same organization name. Named by port they are legible;
	// named local / local-2 / local-3 they are a guessing game.
	t.Run("local backends are distinguished by port", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{}}
		for _, port := range []string{"8081", "8082", "8083"} {
			url := "http://localhost:" + port
			name := f.AutoName(url, "development", "")
			if name != "local-"+port {
				t.Errorf("AutoName(%s) = %q, want local-%s", url, name, port)
			}
			f.Profiles[name] = &Profile{URL: url}
		}
		if len(f.Profiles) != 3 {
			t.Fatalf("stored %d profiles, want one per port", len(f.Profiles))
		}
		// And none of them fell back to a bare numeric suffix, which would be a
		// count of collisions rather than a fact about the server.
		for _, bad := range []string{"local", "local-2", "local-3"} {
			if _, taken := f.Profiles[bad]; taken {
				t.Errorf("a profile was named %q — the port was lost", bad)
			}
		}
	})

	// The multi-environment case: one organization across several instances,
	// where an org-derived name says nothing about which one you are on.
	t.Run("same org on another URL falls back to the host label", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{
			"acme-retail": {URL: prod, TenantID: "t1"},
		}}
		if got := f.AutoName(staging, "Acme Retail", ""); got != "staging" {
			t.Errorf("AutoName = %q, want staging", got)
		}
	})

	// Two organizations on ONE instance is the case profiles exist for, and
	// there the org IS the distinguishing axis.
	t.Run("second org on the same URL keeps its own name", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{
			"acme-retail": {URL: prod, TenantID: "t1"},
		}}
		if got := f.AutoName(prod, "Northwind AB", ""); got != "northwind-ab" {
			t.Errorf("AutoName = %q, want northwind-ab", got)
		}
	})

	t.Run("collisions suffix", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{
			"staging":   {URL: staging, TenantID: "t1"},
			"staging-2": {URL: staging, TenantID: "t2"},
		}}
		if got := f.AutoName(staging, "", ""); got != "staging-3" {
			t.Errorf("AutoName = %q, want staging-3", got)
		}
	})

	// A profile being renamed must not collide with itself and become "acme-2".
	t.Run("exclude keeps a profile from colliding with itself", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{
			"app": {URL: prod, TenantID: "t1"},
		}}
		if got := f.AutoName(prod, "Acme", "app"); got != "acme" {
			t.Errorf("AutoName = %q, want acme", got)
		}
		if got := f.AutoName(prod, "App", "app"); got != "app" {
			t.Errorf("AutoName = %q, want the profile to keep its own name", got)
		}
	})

	// An organization whose name slugs to nothing must still produce a usable
	// profile name.
	t.Run("unusable org name falls back", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{}}
		if got := f.AutoName(prod, "日本", ""); got != "app" {
			t.Errorf("AutoName = %q, want the host label app", got)
		}
	})

	// Every derived name must be one the user can actually type back.
	t.Run("derived names are always valid", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{}}
		for _, org := range []string{"Acme Retail", "日本", "", "Ässä Oy", "The Very Long Name Of Some Nordic Holding Company AB"} {
			name := f.AutoName(prod, org, "")
			if err := ValidateName(name); err != nil {
				t.Errorf("AutoName(%q) = %q, which is not a valid name: %v", org, name, err)
			}
			f.Profiles[name] = &Profile{URL: prod}
		}
	})
}

// A conflict between a typed flag and an exported variable resolves in favour
// of the flag; only a conflict WITHIN one layer is refused. Erroring on
// `--profile acme` in a shell that happens to export RONJA_URL would punish the
// user for being specific.
func TestConflictResolvesByLayer(t *testing.T) {
	isolate(t)
	prod := login(t, "https://app.ronja.tech", "ten-prod", "Acme", "prod-token", "2026-01-01T00:00:00Z")
	login(t, "http://localhost:8080", "ten-dev", "", "dev-token", "2026-02-01T00:00:00Z")

	// --profile flag beats an ambient RONJA_URL.
	t.Setenv(EnvURL, "http://localhost:8080")
	resolved, err := Resolve("", prod)
	if err != nil {
		t.Fatalf("--profile against an exported RONJA_URL: %v", err)
	}
	if resolved.Token != "prod-token" {
		t.Errorf("token = %q, want the flag's profile to win over the exported URL", resolved.Token)
	}

	// --url flag beats an ambient RONJA_PROFILE.
	t.Setenv(EnvURL, "")
	t.Setenv(EnvProfile, prod)
	resolved, err = Resolve("http://localhost:8080", "")
	if err != nil {
		t.Fatalf("--url against an exported RONJA_PROFILE: %v", err)
	}
	if resolved.Token != "dev-token" {
		t.Errorf("token = %q, want the flag's URL to win over the exported profile", resolved.Token)
	}

	// Both exported and disagreeing is a misconfigured shell, not a preference.
	t.Setenv(EnvURL, "http://localhost:8080")
	t.Setenv(EnvProfile, prod)
	var mismatch *ProfileURLMismatchError
	if _, err := Resolve("", ""); !errors.As(err, &mismatch) {
		t.Errorf("two conflicting environment variables = %v, want a mismatch error", err)
	}

	// Both typed and disagreeing is refusing to guess which half was meant.
	t.Setenv(EnvURL, "")
	t.Setenv(EnvProfile, "")
	if _, err := Resolve("http://localhost:8080", prod); !errors.As(err, &mismatch) {
		t.Errorf("two conflicting flags = %v, want a mismatch error", err)
	}
}
