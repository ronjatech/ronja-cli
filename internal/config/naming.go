package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Profile names are handles, not identity — but they are also map keys, command
// arguments and the subject of error messages, so they are kept to a shape that
// survives all three: lowercase ASCII letters, digits and single hyphens.
//
// stemMax bounds a DERIVED name, so an organization called "The Very Long Name
// Of Some Nordic Holding Company AB" does not become the thing you have to type.
// nameMax bounds any name, derived or typed, and exists only to keep the file
// legible — nothing breaks at 65 characters.
const (
	stemMax = 32
	nameMax = 64
)

// ValidateName reports whether a user-supplied profile name is usable.
//
// Typed names are VALIDATED rather than silently slugged, which derived names
// are. Quietly turning `--profile "Acme Prod"` into `acme-prod` means the name
// the user asked for is not the name any later command will accept, and they
// find that out from a "no such profile" much later.
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

// Slug turns a display name — an organization's, usually — into a profile name.
//
// Anything outside the allowed set is DROPPED rather than transliterated: an
// organization named "Ässä Oy" becomes "ss-oy", which is a poor name but a
// usable one, and the user can rename it. Guessing at transliteration for every
// script would be a lot of table for a string nobody is required to keep.
func Slug(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == ' ' || r == '-' || r == '_' || r == '.' || r == '/':
			// Collapse runs, so "Acme  --  Retail" is not "acme----retail".
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
// "http://localhost:8082" → "local-8082".
//
// The first DNS label rather than the whole host, because the distinguishing
// part of an instance URL is its subdomain — every Ronja instance shares the
// rest.
//
// The PORT is part of the name whenever it is not the scheme's default, and on
// loopback that is the whole point: several local backends at once is the
// normal way to work on Ronja (one per worktree, on 8081, 8082, 8083…), and
// they are identical in every respect a name could otherwise draw on. Without
// the port they all derive "local" and collide into local-2, local-3 — numbers
// that say nothing about which server they reach, in the one situation where
// picking the wrong one is easiest.
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

// hostStem is the naming half of a host, before the port is considered.
func hostStem(host string) string {
	if isLoopbackAuthority(host) {
		return "local"
	}
	// An IP address has no subdomain to take — its dots are not a hierarchy, so
	// splitting on the first one turns 10.0.0.7 into "10".
	if net.ParseIP(host) != nil {
		return Slug(host)
	}
	if label, _, found := strings.Cut(host, "."); found && label != "" {
		return Slug(label)
	}
	return Slug(host)
}

// isDefaultPort reports whether a port is the one the scheme implies, and so
// says nothing worth putting in a name. An explicitly written ":443" and an
// omitted one are the same instance.
func isDefaultPort(scheme, port string) bool {
	return (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
}

// AutoName derives a profile name for a login, unique within the store.
//
// The organization is the right name for the case profiles exist to serve —
// several organizations on ONE instance, where the host is identical and the
// org is the only thing that distinguishes them. Two cases invert that:
//
//   - A LOOPBACK instance. A dev's local backend and production frequently
//     carry the same organization name, and "local" is what they actually call
//     it.
//   - A same-named organization already stored on a DIFFERENT URL. That is the
//     multi-environment case (one org, local/staging/production), where the URL
//     is the distinguishing axis and an org-derived name says nothing.
//
// exclude is the profile being renamed, if any, so a profile does not collide
// with itself and end up as "acme-2".
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

// Allocate returns stem, or the first free stem-2, stem-3, … .
//
// MUST be called inside the store lock, together with the write that takes the
// name. Allocating and then writing as two separate locked steps is the same
// race one layer up: two fresh logins both find "app" free, and the second one
// evicts the first's token — which is the entire bug profiles exist to fix.
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

// isLoopbackURL reports whether a full URL points at this machine.
func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return isLoopbackAuthority(u.Hostname())
}
