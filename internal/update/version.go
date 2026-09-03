package update

import (
	"strconv"
	"strings"
)

// Version comparison, hand-rolled.
//
// The CLI has three dependencies and this needs one shape of version string:
// `vMAJOR.MINOR.PATCH` with an optional prerelease suffix, which is exactly
// what the release pipeline stamps (`-X ...Version={{ .Tag }}`). Pulling in a
// semver module to compare four integers would be a dependency added for a
// twenty-line function.
//
// Everything that is NOT that shape reports ok=false, and ok=false means the
// caller stays silent: a `dev` build from a checkout, Go's `(devel)`, and the
// pseudo-version a `go install ...@<commit>` records all land here. None of
// them can be meaningfully compared against a release tag, and guessing would
// tell a developer running their own build that they are behind.

type parsedVersion struct {
	nums [3]int
	pre  string
}

// Compare reports whether latest is newer than current.
//
// ok is false unless BOTH strings are release versions; a caller must treat
// ok=false as "no opinion", never as "not newer".
func Compare(current, latest string) (newer, ok bool) {
	c, okC := parseVersion(current)
	l, okL := parseVersion(latest)
	if !okC || !okL {
		return false, false
	}
	for i := range c.nums {
		if l.nums[i] != c.nums[i] {
			return l.nums[i] > c.nums[i], true
		}
	}
	// Same numeric core, so the prerelease decides. A prerelease sorts BELOW
	// its own release, which is the case that matters: someone running
	// v0.29.0-rc.1 is told about v0.29.0.
	switch {
	case c.pre == l.pre:
		return false, true
	case c.pre != "" && l.pre == "":
		return true, true
	case c.pre == "" && l.pre != "":
		return false, true
	default:
		// Two prereleases of the same core. A byte comparison is not the
		// semver rule (rc.10 sorts below rc.9), and it does not need to be:
		// the answer only decides whether to mention a release candidate to
		// someone already running one.
		return l.pre > c.pre, true
	}
}

// IsRelease reports whether v is a version this package can compare — i.e.
// whether the running binary came from the release pipeline at all.
func IsRelease(v string) bool {
	_, ok := parseVersion(v)
	return ok
}

func parseVersion(v string) (parsedVersion, bool) {
	var out parsedVersion
	if !strings.HasPrefix(v, "v") {
		return out, false
	}
	body := v[1:]
	// Build metadata is not something the pipeline produces, and comparing it
	// is explicitly not a thing semver does. Refuse rather than half-handle it.
	if strings.Contains(body, "+") {
		return out, false
	}
	if i := strings.IndexByte(body, '-'); i >= 0 {
		out.pre = body[i+1:]
		body = body[:i]
		if !validPrerelease(out.pre) {
			return out, false
		}
	}
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		// strconv.Atoi accepts "+1" and "-1"; a version component is digits.
		if p == "" || strings.TrimLeft(p, "0123456789") != "" {
			return out, false
		}
		// Semver forbids a leading zero, and accepting one would make v0.01.0
		// and v0.1.0 compare equal while printing as two different releases —
		// so a tag that is not a version we could ever have published is
		// refused rather than quietly normalised.
		if len(p) > 1 && p[0] == '0' {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out.nums[i] = n
	}
	if isPseudoVersion(out.pre) {
		return out, false
	}
	return out, true
}

// validPrerelease accepts semver's prerelease grammar and nothing else:
// dot-separated identifiers of [0-9A-Za-z-], none of them empty.
//
// It is a PRINTING rule as much as a parsing one. The tag comes from whatever
// the mirror answered, and `ok` from Compare is the gate that admits it into
// the notice line and into `ronja update`'s report — both of which go to a
// terminal verbatim. Without a charset test, a tag carrying a bidirectional
// override or a control character would be echoed there, where it can rewrite
// the line around it: "ronja v1.0.0-rc.1<U+202E>..." is a sentence the reader
// does not get to see honestly. The one place a tag is still printed WITHOUT
// passing this gate is the "tagged %q, which is not a release version" refusal,
// and %q escapes what this rejects.
func validPrerelease(pre string) bool {
	if pre == "" {
		return false
	}
	for _, id := range strings.Split(pre, ".") {
		if id == "" {
			return false
		}
		for i := 0; i < len(id); i++ {
			switch c := id[i]; {
			case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// isPseudoVersion recognises the version Go synthesises for a module resolved
// at a commit rather than a tag: v0.0.0-20260901120000-abcdef123456, and its
// two tagged variants, all of which end in a 14-digit UTC timestamp followed by
// a 12-character commit prefix.
//
// It has to be recognised because it PARSES as vMAJOR.MINOR.PATCH-pre and
// would otherwise be compared as an ordinary prerelease of v0.0.0 — telling
// every `go install ...@<commit>` user that a release is available and that
// `ronja update` will fix it, which for that install method it would not.
func isPseudoVersion(pre string) bool {
	// Split on both separators: the timestamp is preceded by a dash in the
	// untagged form (v0.0.0-20260901120000-abcdef123456) and by a dot in the
	// two tagged ones (v0.28.4-0.20260901120000-abcdef123456).
	parts := strings.FieldsFunc(pre, func(r rune) bool { return r == '.' || r == '-' })
	if len(parts) < 2 {
		return false
	}
	stamp, commit := parts[len(parts)-2], parts[len(parts)-1]
	if len(stamp) != 14 || strings.TrimLeft(stamp, "0123456789") != "" {
		return false
	}
	if len(commit) != 12 || strings.TrimLeft(commit, "0123456789abcdef") != "" {
		return false
	}
	return true
}
