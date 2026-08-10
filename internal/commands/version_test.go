package commands

import (
	"runtime/debug"
	"testing"
)

// buildInfo returns a reader standing in for debug.ReadBuildInfo.
func buildInfo(version string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: version}}, true
	}
}

func TestResolveVersionPrefersTheReleaseStamp(t *testing.T) {
	// A pipeline binary carries both a stamp and build info. The stamp is the
	// version actually released, so it must win.
	if got := resolveVersionFrom("v0.26.2", buildInfo("v9.9.9")); got != "v0.26.2" {
		t.Errorf("version = %q, want the ldflags stamp v0.26.2", got)
	}
}

// The regression this exists for: `go install ...@latest` compiles from source
// with no ldflags, so an unstamped binary reported "dev" for a real published
// release and a bug report could not say which one.
func TestResolveVersionFallsBackToTheModuleVersion(t *testing.T) {
	if got := resolveVersionFrom("dev", buildInfo("v0.26.2-rc.1")); got != "v0.26.2-rc.1" {
		t.Errorf("version = %q, want the resolved module version v0.26.2-rc.1 — "+
			"a `go install` build cannot identify itself otherwise", got)
	}
}

func TestResolveVersionKeepsDevForALocalBuild(t *testing.T) {
	// "(devel)" is what a `go build` from a checkout reports. Reporting it
	// would be no more informative than "dev" and less recognisable.
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
