package wfdir

import (
	"fmt"

	"github.com/ronjatech/ronja-cli/internal/config"
)

// InstanceKey identifies what a folder is bound TO: one instance and one
// organization on it.
//
// The organization is part of the key because workflow ids are scoped to one.
// Keyed by URL alone, a person who belongs to two organizations on the same
// instance has one entry for both — so a push made while signed in to the
// second sends the FIRST one's workflow id under the second's token. That does
// not fail safely: the id is simply not found there, and the push path treats
// not-found as "create it", quietly producing a duplicate workflow in the wrong
// organization.
//
// It is deliberately NOT keyed by profile NAME. ronja.json is committed and
// shared, and a profile name is local to one machine — my "prod" is not yours.
//
// TenantID may be empty, meaning "not known right now" — a signed-out `wf
// status` has no way to learn it. See Find for how that degrades.
type InstanceKey struct {
	URL      string
	TenantID string
}

// Known reports whether the key names an organization.
func (k InstanceKey) Known() bool { return k.TenantID != "" }

// ErrAmbiguousInstance is returned when the caller's organization is unknown
// and the folder is bound to several on one instance. Picking one would be a
// coin flip between two organizations' workflows.
var ErrAmbiguousInstance = fmt.Errorf("this folder is bound to several organizations")

// Find locates the entry matching want, returning its index or -1.
//
// Two rules, and the second is the interesting one:
//
//   - A key that NAMES an organization matches only an entry with that exact
//     organization. It never adopts an entry belonging to another, and never
//     adopts an untagged one — a hand-written entry with no organization could
//     belong to anybody.
//   - A key with an UNKNOWN organization (signed out) matches on URL alone when
//     that is unambiguous, and errors when it is not. Without this a bound
//     folder would report itself unbound to a signed-out `wf status`, which is
//     a read-only command that used to work fine.
//
// URLs are compared through config.NormalizeURL, because ronja.json is a
// committed file people edit by hand and the two spellings they reach for — a
// trailing slash and an uppercase hostname — are exactly what it folds away. A
// raw comparison misses those, and the folder then silently reports as unbound
// against the instance it is plainly bound to, which a first push would "fix"
// by creating a second workflow.
func Find[T any](items []T, keyOf func(T) InstanceKey, want InstanceKey) (int, error) {
	wantURL, err := config.NormalizeURL(want.URL)
	if err != nil {
		return -1, nil
	}

	onURL := []int{}
	for i, item := range items {
		key := keyOf(item)
		norm, err := config.NormalizeURL(key.URL)
		if err != nil || norm != wantURL {
			continue
		}
		if want.Known() {
			if key.TenantID == want.TenantID {
				return i, nil
			}
			continue
		}
		onURL = append(onURL, i)
	}

	switch {
	case !want.Known() && len(onURL) == 1:
		return onURL[0], nil
	case !want.Known() && len(onURL) > 1:
		return -1, ErrAmbiguousInstance
	}
	return -1, nil
}

// Matches reports whether two keys name the same instance and organization,
// tolerating URL spelling the same way Find does. Used when writing, to replace
// an equivalent entry rather than accumulate a second one beside it.
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
