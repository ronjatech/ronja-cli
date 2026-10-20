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
		{"non-ascii dropped", "Ässä Oy", "ss-oy"},
		{"entirely non-ascii", "日本", ""},
		{"empty", "", ""},
		{"separators only", " _-. ", ""},
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
		{"http://localhost:8080", "local-8080"},
		{"http://localhost:8082", "local-8082"},
		{"http://127.0.0.1:8081", "local-8081"},
		{"http://[::1]:8083", "local-8083"},
		{"http://localhost", "local"},
		{"https://app.ronja.tech:443", "app"},
		{"http://localhost:80", "local"},
		{"https://corp.example:8443", "corp-8443"},
		{"https://corp.example/ronja", "corp"},
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

	t.Run("loopback prefers the host label", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{}}
		if got := f.AutoName(local, "Development", ""); got != "local-8080" {
			t.Errorf("AutoName = %q, want local-8080", got)
		}
	})

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
		for _, bad := range []string{"local", "local-2", "local-3"} {
			if _, taken := f.Profiles[bad]; taken {
				t.Errorf("a profile was named %q — the port was lost", bad)
			}
		}
	})

	t.Run("same org on another URL falls back to the host label", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{
			"acme-retail": {URL: prod, TenantID: "t1"},
		}}
		if got := f.AutoName(staging, "Acme Retail", ""); got != "staging" {
			t.Errorf("AutoName = %q, want staging", got)
		}
	})

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

	t.Run("unusable org name falls back", func(t *testing.T) {
		f := &File{Profiles: map[string]*Profile{}}
		if got := f.AutoName(prod, "日本", ""); got != "app" {
			t.Errorf("AutoName = %q, want the host label app", got)
		}
	})

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

// Only a conflict within one layer is refused; a typed flag beats an exported
// variable, or `--profile acme` would fail in any shell exporting RONJA_URL.
func TestConflictResolvesByLayer(t *testing.T) {
	isolate(t)
	prod := login(t, "https://app.ronja.tech", "ten-prod", "Acme", "prod-token", "2026-01-01T00:00:00Z")
	login(t, "http://localhost:8080", "ten-dev", "", "dev-token", "2026-02-01T00:00:00Z")

	t.Setenv(EnvURL, "http://localhost:8080")
	resolved, err := Resolve("", prod)
	if err != nil {
		t.Fatalf("--profile against an exported RONJA_URL: %v", err)
	}
	if resolved.Token != "prod-token" {
		t.Errorf("token = %q, want the flag's profile to win over the exported URL", resolved.Token)
	}

	t.Setenv(EnvURL, "")
	t.Setenv(EnvProfile, prod)
	resolved, err = Resolve("http://localhost:8080", "")
	if err != nil {
		t.Fatalf("--url against an exported RONJA_PROFILE: %v", err)
	}
	if resolved.Token != "dev-token" {
		t.Errorf("token = %q, want the flag's URL to win over the exported profile", resolved.Token)
	}

	t.Setenv(EnvURL, "http://localhost:8080")
	t.Setenv(EnvProfile, prod)
	var mismatch *ProfileURLMismatchError
	if _, err := Resolve("", ""); !errors.As(err, &mismatch) {
		t.Errorf("two conflicting environment variables = %v, want a mismatch error", err)
	}

	t.Setenv(EnvURL, "")
	t.Setenv(EnvProfile, "")
	if _, err := Resolve("http://localhost:8080", prod); !errors.As(err, &mismatch) {
		t.Errorf("two conflicting flags = %v, want a mismatch error", err)
	}
}
