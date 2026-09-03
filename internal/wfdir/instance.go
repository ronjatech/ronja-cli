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
	matches := FindAll(items, keyOf, want)
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) == 0:
		return -1, nil
	case !want.Known():
		return -1, ErrAmbiguousInstance
	default:
		// Several EXACT matches on one (instance, organization). The shapes this
		// searches are all supposed to hold at most one — SetBinding replaces
		// rather than appends, and Manifest.selectNamed refuses to CREATE a
		// second stack on an organization this folder already names — so
		// reaching here means a hand-edited or merged file, which is not
		// refused at load precisely so the folder stays openable (see
		// checkStacks). The FIRST is answered rather than an error, because that is what
		// this has always done and every caller of Find treats "no entry" as
		// "never pushed here", which for a duplicate would create a second
		// resource. Manifest.Select is where the duplicate is reported.
		return matches[0], nil
	}
}

// FindAll lists every entry matching want, by exactly the rules Find documents
// — which is the point of it existing: Find is written in terms of this, so the
// two can never disagree about what "matches" means.
//
// It reports MATCHES, not a verdict. Deciding that two of them is an ambiguity
// worth refusing is the caller's, because the two callers disagree: Find's
// contract is one answer or none, while Manifest.Select has a --stack flag to
// offer and can name what it found.
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
//
// It folds URL spelling exactly as Find does, and lives beside it for that
// reason. Two callers depend on the two agreeing: the refusal that enumerates
// the organizations a folder names on an instance, and the check that decides
// whether resolving the caller's own organization is worth a round trip. A
// rawer comparison here would name a different set from the one the lookup saw
// — the worst kind of disagreement, since it is invisible until the spellings
// differ.
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
