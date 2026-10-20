package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr lives in workflow_testcmd_test.go.

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
	// The warning must read as a live credential to revoke, not housekeeping.
	for _, want := range []string{"LIVE", "Access tokens"} {
		if !strings.Contains(warned, want) {
			t.Errorf("the warning does not mention %q — it reads as housekeeping, not a live credential: %q", want, warned)
		}
	}

	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	if quiet := captureStderr(t, warnLegacyStore); quiet != "" {
		t.Errorf("warned with no legacy file present: %q", quiet)
	}
}
