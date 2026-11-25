package wfdir

import (
	"os"
	"path/filepath"
	"testing"
)

// One person in two organizations on one instance: each organization's test
// app is remembered on its own, so neither id is ever sent to the other.
func TestScratchAppIsKeptPerOrganization(t *testing.T) {
	root := t.TempDir()
	orgA := InstanceKey{URL: "https://ronja.example", TenantID: "tenant-a"}
	orgB := InstanceKey{URL: "https://ronja.example/", TenantID: "tenant-b"}

	if err := RecordScratchApp(root, orgA, "data_app-a"); err != nil {
		t.Fatal(err)
	}
	if got := LoadScratchApp(root, orgB); got != "" {
		t.Errorf("organization B reads %q, want none", got)
	}
	if err := RecordScratchApp(root, orgB, "data_app-b"); err != nil {
		t.Fatal(err)
	}
	if got := LoadScratchApp(root, orgA); got != "data_app-a" {
		t.Errorf("organization A reads %q after B recorded its own, want data_app-a", got)
	}
	if got := LoadScratchApp(root, orgB); got != "data_app-b" {
		t.Errorf("organization B reads %q, want data_app-b", got)
	}
}

// A folder reached through a symlink is the same folder; one at another path
// holding the same file is not.
func TestScratchAppFollowsTheFolderNotItsSpelling(t *testing.T) {
	key := InstanceKey{URL: "https://ronja.example", TenantID: "tenant-a"}
	root := t.TempDir()
	if err := RecordScratchApp(root, key, "data_app-a"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if got := LoadScratchApp(link, key); got != "data_app-a" {
		t.Errorf("through a symlink the folder reads %q, want data_app-a", got)
	}

	moved := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(root, StateDirName, ScratchFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(moved, StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moved, StateDirName, ScratchFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadScratchApp(moved, key); got != "" {
		t.Errorf("a copy at another path reads %q, want none", got)
	}
}
