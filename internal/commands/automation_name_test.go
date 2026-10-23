package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// An automation's NAME: optional in the file, managed when present, and — the
// behaviour change — never re-patched to the filename when absent.

// cronRow is a live row that already matches cronFile in every declared field.
func cronRow(name string) *api.Automation {
	return &api.Automation{ID: "job-1", Name: name, CronExpr: "0 2 * * *", Prompt: "Refresh the nightly aggregates.", Enabled: true}
}

func namedCronFile(name string) string {
	return `{"name": "` + name + `", "triggerKind": "cron", "cronExpr": "0 2 * * *", "prompt": "Refresh the nightly aggregates."}`
}

func automationNameIn(t *testing.T, root string) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	return lock.AutomationName("prod", "nightly.json")
}

func automationNameWrites(f *fakeAutomationInstance) int {
	n := 0
	for _, u := range f.updates {
		if _, ok := u.Body["name"]; ok {
			n++
		}
	}
	return n
}

// A file with NO "name" against an automation renamed in the web app: the
// rename stands. Every push used to patch the name back to the filename.
func TestAutomationPushLeavesAnUndeclaredNameAlone(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(cronRow("Renamed in the web app"))
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"}, map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	out, stderr, err := runAutomationCLI(t, dir, "automation", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if got := f.RowOf("job-1").Name; got != "Renamed in the web app" {
		t.Errorf("an undeclared name was patched to %q", got)
	}
	if n := automationNameWrites(f); n != 0 {
		t.Errorf("%d write(s) carried a name the file does not declare", n)
	}
	if !strings.Contains(out, automationOutcomeUnchanged) {
		t.Errorf("a row matching every declared field was not reported unchanged:\n%s", out)
	}
	// The report names the automation by the name it HAS, not the filename the
	// push no longer writes.
	if !strings.Contains(out, `"name": "Renamed in the web app"`) {
		t.Errorf("--json does not report the automation's own name:\n%s", out)
	}
}

// A DECLARED name renames the same row, with the filename as the baseline when
// nothing is recorded, and the applied name is recorded.
func TestAutomationPushRenamesToTheDeclaredName(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(cronRow("nightly"))
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"}, map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": namedCronFile("Nightly refresh")})

	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if got := f.RowOf("job-1").Name; got != "Nightly refresh" {
		t.Fatalf("name = %q", got)
	}
	if got := automationNameIn(t, dir); got != "Nightly refresh" {
		t.Errorf("lock records %q", got)
	}
}

// RENAMED ELSEWHERE IS DRIFT for a declared name, even with no updatedAt anchor
// to catch it — and --force renames it back.
func TestAutomationPushRefusesANameRenamedElsewhere(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(cronRow("Web name"))
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1", map[string]string{"nightly.json": "job-1"}, nil)
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": namedCronFile("Nightly refresh")})

	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a renamed automation was renamed back without --force")
	}
	if got := f.RowOf("job-1").Name; got != "Web name" {
		t.Fatalf("name = %q", got)
	}
	if !strings.Contains(stderr, `"Web name"`) {
		t.Errorf("the refusal does not name the live name:\n%s", stderr)
	}
	if _, _, err := runAutomationCLI(t, dir, "automation", "push", "--force"); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if got := f.RowOf("job-1").Name; got != "Nightly refresh" {
		t.Errorf("--force did not rename: %q", got)
	}
}

// LIVE EQUALS DESIRED IS NEVER DRIFT. The row moved since the anchor — the
// anchor write after an earlier push was lost — but it holds exactly what the
// file declares, name included: nothing to overwrite, so no refusal, and the
// anchor and the name are re-taken from the row.
func TestAutomationPushReanchorsARowThatAlreadyMatches(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(cronRow("Nightly refresh"))
	f.Touch("job-1", laterTime)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"}, map[string]string{"nightly.json": anchoredAt(baseTime)})
	lock.SetAutomationName("prod", "nightly.json", "job-1", "A stale name anchor")
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": namedCronFile("Nightly refresh")})

	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("push refused a row that already matches the file: %v\n%s", err, stderr)
	}
	if len(f.updates) != 0 {
		t.Errorf("a row that already matched was written: %+v", f.updates)
	}
	if got := automationSeenOf(t, dir, "prod", "nightly.json"); got != anchoredAt(laterTime) {
		t.Errorf("anchor = %q, want it re-taken from the row", got)
	}
	if got := automationNameIn(t, dir); got != "Nightly refresh" {
		t.Errorf("name anchor = %q, want it re-taken", got)
	}
}

// A NEW FILE WITH A DECLARED NAME is created under it.
func TestAutomationPushCreatesUnderTheDeclaredName(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": namedCronFile("Nightly refresh")})
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.created) != 1 || f.created[0].Name != "Nightly refresh" {
		t.Fatalf("created = %+v", f.created)
	}
	if got := automationNameIn(t, dir); got != "Nightly refresh" {
		t.Errorf("lock records %q", got)
	}
}

// An empty "name" is refused at parse rather than sent.
func TestAutomationFileRefusesAnEmptyName(t *testing.T) {
	if _, err := parseAutomationFile("a.json", `{"name": "  "}`); err == nil {
		t.Error("an empty name was accepted")
	}
	file, err := parseAutomationFile("a.json", `{"name": " Nightly "}`)
	if err != nil || file.Name == nil || *file.Name != "Nightly" {
		t.Errorf("name = %v, %v", file, err)
	}
}
