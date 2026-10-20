package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Profile names are lowercase ASCII letters, digits and single hyphens.
// stemMax bounds a derived name so it stays typeable; nameMax bounds any name
// for legibility only.
const (
	stemMax = 32
	nameMax = 64
)

// ValidateName reports whether a user-supplied profile name is usable. Typed
// names are validated, never slugged, so the name asked for is the name later
// commands accept.
func ValidateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a profile name cannot be empty")
	case len(name) > nameMax:
		return fmt.Errorf("profile name %q is longer than %d characters", name, nameMax)
	case strings.HasPrefix(name, "-"), strings.HasSuffix(name, "-"):
		return fmt.Errorf("profile name %q cannot start or end with a hyphen", name)
	}
	for _, r := range name {
		if !isNameRune(r) {
			return fmt.Errorf("profile name %q may only contain lowercase letters, digits and hyphens (%q is not allowed)",
				name, string(r))
		}
	}
	return nil
}

func isNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
}

// Slug turns a display name, usually an organization's, into a profile name.
// Runes outside the allowed set are dropped, not transliterated ("Ässä Oy" →
// "ss-oy"): poor but usable, and renameable.
func Slug(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == ' ' || r == '-' || r == '_' || r == '.' || r == '/':
			if !prevHyphen && b.Len() > 0 {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > stemMax {
		out = strings.Trim(out[:stemMax], "-")
	}
	return out
}

// HostLabel derives a name from an instance URL, for the cases where the
// organization cannot supply one: "https://staging.ronja.tech" → "staging",
// "http://localhost:8082" → "local-8082". A non-default port is part of the
// name: several local backends (one per worktree) are otherwise identical.
func HostLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	label := hostStem(u.Hostname())
	if port := u.Port(); port != "" && !isDefaultPort(u.Scheme, port) {
		label = strings.Trim(label+"-"+Slug(port), "-")
	}
	return label
}

func hostStem(host string) string {
	if isLoopbackAuthority(host) {
		return "local"
	}
	// An IP's dots are not a hierarchy; splitting would turn 10.0.0.7 into "10".
	if net.ParseIP(host) != nil {
		return Slug(host)
	}
	if label, _, found := strings.Cut(host, "."); found && label != "" {
		return Slug(label)
	}
	return Slug(host)
}

func isDefaultPort(scheme, port string) bool {
	return (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
}

// AutoName derives a profile name for a login, unique within the store: the
// organization's, except on loopback or when that name is held by a profile on
// a different URL, where the host label distinguishes better. exclude is the
// profile being renamed, so it does not collide with itself.
func (f *File) AutoName(instanceURL, tenantName, exclude string) string {
	stem := Slug(tenantName)
	if stem == "" || isLoopbackURL(instanceURL) {
		stem = HostLabel(instanceURL)
	} else if p, taken := f.Profiles[stem]; taken && p != nil && p.URL != instanceURL && stem != exclude {
		stem = HostLabel(instanceURL)
	}
	if stem == "" {
		stem = "profile"
	}
	return f.Allocate(stem, exclude)
}

// Allocate returns stem, or the first free stem-2, stem-3, … . It must run in
// the same Update as the write that takes the name, or two logins can both
// find a name free and the second evicts the first's token.
func (f *File) Allocate(stem, exclude string) string {
	if !f.taken(stem, exclude) {
		return stem
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", stem, i)
		if !f.taken(candidate, exclude) {
			return candidate
		}
	}
}

func (f *File) taken(name, exclude string) bool {
	if name == exclude {
		return false
	}
	_, ok := f.Profiles[name]
	return ok
}

func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return isLoopbackAuthority(u.Hostname())
}
