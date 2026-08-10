package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr lives in workflow_testcmd_test.go.

// The abandoned pre-profiles store still holds a live, full-access token that
// this CLI can no longer see — `ronja logout` cannot clear it. Not migrating it
// was a decision; leaving it unmentioned would not be.
func TestLegacyStoreIsReportedWhenPresent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)
	t.Setenv("RONJA_URL", "")
	t.Setenv("RONJA_TOKEN", "")
	t.Setenv("RONJA_PROFILE", "")

	legacy := filepath.Join(dir, legacyStoreName)
	if err := os.WriteFile(legacy, []byte(`{"hosts":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	warned := captureStderr(t, warnLegacyStore)
	if !strings.Contains(warned, legacy) {
		t.Errorf("the warning does not name the file: %q", warned)
	}
	// The point is the credential, not the tidiness. A note that only said "an
	// old file is here" would not tell anyone to go revoke anything.
	for _, want := range []string{"LIVE", "Access tokens"} {
		if !strings.Contains(warned, want) {
			t.Errorf("the warning does not mention %q — it reads as housekeeping, not a live credential: %q", want, warned)
		}
	}

	// And it stays quiet once the file is gone, or it becomes noise that gets
	// ignored on the one machine where it matters.
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	if quiet := captureStderr(t, warnLegacyStore); quiet != "" {
		t.Errorf("warned with no legacy file present: %q", quiet)
	}
}
