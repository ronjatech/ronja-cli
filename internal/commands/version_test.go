package commands

import (
	"runtime/debug"
	"testing"
)

func buildInfo(version string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: version}}, true
	}
}

func TestResolveVersionPrefersTheReleaseStamp(t *testing.T) {
	if got := resolveVersionFrom("v0.26.2", buildInfo("v9.9.9")); got != "v0.26.2" {
		t.Errorf("version = %q, want the ldflags stamp v0.26.2", got)
	}
}

// `go install ...@latest` builds with no ldflags, so the module version is the
// only way such a binary can identify itself.
func TestResolveVersionFallsBackToTheModuleVersion(t *testing.T) {
	if got := resolveVersionFrom("dev", buildInfo("v0.26.2-rc.1")); got != "v0.26.2-rc.1" {
		t.Errorf("version = %q, want the resolved module version v0.26.2-rc.1 — "+
			"a `go install` build cannot identify itself otherwise", got)
	}
}

func TestResolveVersionKeepsDevForALocalBuild(t *testing.T) {
	// "(devel)" is what a `go build` from a checkout reports.
	if got := resolveVersionFrom("dev", buildInfo("(devel)")); got != "dev" {
		t.Errorf("version = %q, want dev for a local build", got)
	}
	if got := resolveVersionFrom("dev", buildInfo("")); got != "dev" {
		t.Errorf("version = %q, want dev when build info carries no version", got)
	}
	missing := func() (*debug.BuildInfo, bool) { return nil, false }
	if got := resolveVersionFrom("dev", missing); got != "dev" {
		t.Errorf("version = %q, want dev when build info is unavailable", got)
	}
}
