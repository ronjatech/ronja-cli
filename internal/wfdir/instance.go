package wfdir

import (
	"fmt"

	"github.com/ronjatech/ronja-cli/internal/config"
)

// InstanceKey identifies what a folder is bound TO: one instance and one
// organization on it.
//
// The organization is part of the key because resource ids are scoped to one:
// keyed by URL alone, a push signed in to a second organization would send the
// first's id, and the push treats not-found as "create it". It is deliberately
// not keyed by profile name, which is local to one machine while ronja.json is
// committed. An empty TenantID means "not known right now"; see Find.
type InstanceKey struct {
	URL      string
	TenantID string
}

// Known reports whether the key names an organization.
func (k InstanceKey) Known() bool { return k.TenantID != "" }

// ErrAmbiguousInstance is returned when the caller's organization is unknown
// and the folder is bound to several on one instance.
var ErrAmbiguousInstance = fmt.Errorf("this folder is bound to several organizations")

// Find locates the entry matching want, returning its index or -1.
//
// A key that names an organization matches only an entry with that exact
// organization, never another's or an untagged one (a hand-written entry could
// belong to anybody). A key with an unknown organization matches on URL alone
// when that is unambiguous and returns ErrAmbiguousInstance when it is not.
// URLs are compared through config.NormalizeURL because ronja.json is
// hand-edited, and a missed match would make a push create a second resource.
func Find[T any](items []T, keyOf func(T) InstanceKey, want InstanceKey) (int, error) {
	matches := FindAll(items, keyOf, want)
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) == 0:
		return -1, nil
	case !want.Known():
		return -1, ErrAmbiguousInstance
	default:
		// Several exact matches means a hand-edited or merged file. The first is
		// answered rather than an error because every caller treats "no entry" as
		// "never pushed here" and would create a second resource;
		// Manifest.Select reports the duplicate. Duplicates are tolerated at
		// load (checkStacks) so a hand-merged folder stays openable.
		return matches[0], nil
	}
}

// FindAll lists every entry matching want, by the rules Find documents. It
// reports matches, not a verdict: whether several is an ambiguity is the
// caller's call.
func FindAll[T any](items []T, keyOf func(T) InstanceKey, want InstanceKey) []int {
	wantURL, err := config.NormalizeURL(want.URL)
	if err != nil {
		return nil
	}

	var exact, onURL []int
	for i, item := range items {
		key := keyOf(item)
		norm, err := config.NormalizeURL(key.URL)
		if err != nil || norm != wantURL {
			continue
		}
		if want.Known() {
			if key.TenantID == want.TenantID {
				exact = append(exact, i)
			}
			continue
		}
		onURL = append(onURL, i)
	}
	if want.Known() {
		return exact
	}
	return onURL
}

// KeysOn lists the keys of every item bound to one instance, in slice order.
// It must fold URL spelling exactly as Find does, or it names a different set
// from the one the lookup saw.
func KeysOn[T any](items []T, keyOf func(T) InstanceKey, url string) []InstanceKey {
	wantURL, err := config.NormalizeURL(url)
	if err != nil {
		return nil
	}
	var out []InstanceKey
	for _, item := range items {
		key := keyOf(item)
		norm, err := config.NormalizeURL(key.URL)
		if err != nil || norm != wantURL {
			continue
		}
		out = append(out, key)
	}
	return out
}

// Matches reports whether two keys name the same instance and organization,
// tolerating URL spelling the same way Find does.
func Matches(a, b InstanceKey) bool {
	if a.TenantID != b.TenantID {
		return false
	}
	an, err := config.NormalizeURL(a.URL)
	if err != nil {
		return false
	}
	bn, err := config.NormalizeURL(b.URL)
	if err != nil {
		return false
	}
	return an == bn
}
