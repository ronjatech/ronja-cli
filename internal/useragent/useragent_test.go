package useragent

import "testing"

// The format is a contract with the server: it parses `ronja-cli/<version>` to
// count CLI adoption before it moves a default an older CLI cannot follow, and
// reads a bare `ronja-cli` as "older than every versioned build".
func TestForPinsTheFormat(t *testing.T) {
	for version, want := range map[string]string{
		"v0.26.2":                             "ronja-cli/v0.26.2",
		"v0.26.3-0.20261012101010-abcdef0123": "ronja-cli/v0.26.3-0.20261012101010-abcdef0123",
		"dev":                                 "ronja-cli/dev",
		"":                                    "ronja-cli",
		"v1 (patched)":                        "ronja-cli",
	} {
		if got := For(version); got != want {
			t.Errorf("For(%q) = %q, want %q", version, got, want)
		}
	}
}

// `ronja api`'s token is its own, under the same version rule.
func TestForAPIPinsTheFormat(t *testing.T) {
	for version, want := range map[string]string{
		"v0.26.2":      "ronja-cli-api/v0.26.2",
		"dev":          "ronja-cli-api/dev",
		"":             "ronja-cli-api",
		"v1 (patched)": "ronja-cli-api",
	} {
		if got := ForAPI(version); got != want {
			t.Errorf("ForAPI(%q) = %q, want %q", version, got, want)
		}
	}
}
