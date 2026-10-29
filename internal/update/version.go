package update

import (
	"strconv"
	"strings"
)

// Version comparison is hand-rolled for the one shape the release pipeline
// stamps, `vMAJOR.MINOR.PATCH[-pre]`. Anything else (a `dev` build, `(devel)`, a
// pseudo-version) reports ok=false so the caller stays silent.
// Copy in backend/lib/clirelease/version.go; a test there keeps them equal.

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
	// A prerelease sorts below its own release.
	switch {
	case c.pre == l.pre:
		return false, true
	case c.pre != "" && l.pre == "":
		return true, true
	case c.pre == "" && l.pre != "":
		return false, true
	default:
		// Byte order, not semver (rc.10 sorts below rc.9): it only decides
		// whether to mention a release candidate to someone running one.
		return l.pre > c.pre, true
	}
}

// IsRelease reports whether v is a version this package can compare, i.e. a
// release-pipeline build.
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
	// The pipeline never produces build metadata.
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
		// A leading zero would make v0.01.0 and v0.1.0 compare equal.
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

// validPrerelease accepts semver's prerelease grammar only: dot-separated,
// non-empty identifiers of [0-9A-Za-z-].
//
// ⚠️ It is also a printing gate: a tag that passes is echoed verbatim into the
// notice and `ronja update`'s report, so a bidi override or control character
// must never pass. The one unvetted print is the not-a-release refusal, which
// uses %q.
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

// isPseudoVersion recognises a Go pseudo-version (a 14-digit timestamp then a
// 12-character commit, e.g. v0.0.0-20260901120000-abcdef123456), which parses
// as a prerelease and would otherwise be compared as one.
func isPseudoVersion(pre string) bool {
	// The timestamp follows a dash untagged and a dot in the tagged forms
	// (v0.28.4-0.20260901120000-abcdef123456).
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
