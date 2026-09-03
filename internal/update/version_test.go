package update

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		name            string
		current, latest string
		newer, ok       bool
	}{
		{"a newer minor", "v0.28.4", "v0.29.0", true, true},
		{"the same version", "v0.28.4", "v0.28.4", false, true},
		{"an older release", "v0.29.0", "v0.28.4", false, true},
		// The one a lexical compare gets wrong, which is why this is not a
		// string comparison.
		{"double-digit patch", "v0.28.9", "v0.28.10", true, true},
		{"a prerelease is behind its release", "v0.29.0-rc.1", "v0.29.0", true, true},
		{"a release is ahead of its prerelease", "v0.29.0", "v0.29.0-rc.1", false, true},
		{"two prereleases of one core", "v0.29.0-rc.1", "v0.29.0-rc.2", true, true},
		{"a development build", "dev", "v0.29.0", false, false},
		{"go's devel marker", "(devel)", "v0.29.0", false, false},
		{"a pseudo-version", "v0.0.0-20260901120000-abcdef123456", "v0.29.0", false, false},
		{"a tagged pseudo-version", "v0.28.4-0.20260901120000-abcdef123456", "v0.29.0", false, false},
		{"no leading v", "0.28.4", "v0.29.0", false, false},
		{"too few components", "v0.28", "v0.29.0", false, false},
		{"a non-numeric component", "v0.x.4", "v0.29.0", false, false},
		{"build metadata", "v0.28.4+meta", "v0.29.0", false, false},
		// Semver forbids a leading zero, so v0.01.0 is not a tag we could have
		// published; accepting it would make it compare equal to v0.1.0 while
		// printing as a different release.
		{"a leading zero in the current version", "v0.01.0", "v0.29.0", false, false},
		{"a leading zero in the latest version", "v0.28.4", "v0.029.0", false, false},
		{"a bare zero component is fine", "v0.28.4", "v1.0.0", true, true},
		{"an unparseable latest", "v0.28.4", "nightly", false, false},
		{"an empty latest", "v0.28.4", "", false, false},
		// A prerelease is printed to a terminal verbatim once ok is true, so
		// the charset is part of the gate: a bidirectional override in a tag
		// rewrites the notice line around it.
		{"an ordinary prerelease", "v0.28.4", "v1.0.0-rc.1", true, true},
		{"a bidi override in the prerelease", "v0.28.4", "v1.0.0-rc.1‮", false, false},
		{"an empty prerelease", "v0.28.4", "v1.0.0-", false, false},
		{"an empty prerelease identifier", "v0.28.4", "v1.0.0-a..b", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newer, ok := Compare(tc.current, tc.latest)
			if newer != tc.newer || ok != tc.ok {
				t.Errorf("Compare(%q, %q) = (%v, %v), want (%v, %v)",
					tc.current, tc.latest, newer, ok, tc.newer, tc.ok)
			}
		})
	}
}

func TestIsRelease(t *testing.T) {
	for _, v := range []string{"v0.28.4", "v1.0.0", "v0.29.0-rc.1"} {
		if !IsRelease(v) {
			t.Errorf("IsRelease(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"dev", "(devel)", "", "v0.0.0-20260901120000-abcdef123456"} {
		if IsRelease(v) {
			t.Errorf("IsRelease(%q) = true, want false", v)
		}
	}
}
