package commands

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// migrateRepo is a git repository holding a workflow folder whose stack "dev"
// declares Space Dev, and a migrations folder.
func migrateRepo(t *testing.T, url string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	wf := filepath.Join(repo, "workflows", "report")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{
  "formatVersion": 9,
  "kind": "workflow",
  "title": "Report",
  "entrypoint": "main.py",
  "stacks": {
    "main": {"url": %[1]q, "tenantID": %[2]q, "featureID": "feat-1"},
    "dev": {"url": %[1]q, "tenantID": %[2]q, "featureID": "feat-dev", "spaceID": "space-dev", "space": "Dev"}
  }
}
`, url, testTenantID)
	if err := os.WriteFile(filepath.Join(wf, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(repo, "db")
	if err := os.MkdirAll(filepath.Join(db, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "migrations", "0001_leads.sql"), []byte("CREATE TABLE leads (id int);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return db
}

// migrateInstance answers db migrate's requests: a database's Space from
// dbSpace ("forbidden" answers 403), and each applied database id appended to
// applied.
func migrateInstance(t *testing.T, dbSpace map[string]string, applied *[]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(api.HeaderSpace) != "" {
			t.Errorf("%s %s carried a Space", r.Method, r.URL.Path)
		}
		switch path := strings.TrimPrefix(r.URL.Path, "/api/v2/"); {
		case path == "authentication/me":
			writeJSON(w, map[string]any{"user": map[string]any{"id": "usr-1"}, "tenant": map[string]any{"id": testTenantID, "name": "Test Org"}})
		case strings.HasPrefix(path, "space/"):
			id := strings.TrimPrefix(path, "space/")
			writeJSON(w, map[string]any{"id": id, "name": map[string]string{"space-dev": "Dev", "space-main": "Main"}[id]})
		case strings.HasSuffix(path, "/migrations"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "database/"), "/migrations")
			*applied = append(*applied, id)
			writeJSON(w, map[string]any{"databaseID": id, "results": []map[string]any{{"name": "0001_leads", "outcome": "skipped", "seq": 1}}})
		case strings.HasPrefix(path, "database/"):
			id := strings.TrimPrefix(path, "database/")
			if dbSpace[id] == "forbidden" {
				writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
				return
			}
			body := map[string]any{"id": id, "name": id + "-name"}
			if dbSpace[id] != "" {
				body["spaceID"] = dbSpace[id]
			}
			writeJSON(w, body)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	signInTo(t, srv.URL)
	return srv.URL
}

// `db migrate push --stack dev` refuses a database outside the Space stack
// "dev" declares in the repository, applies one inside it, and notes an
// instance that does not say; a stack that declares no Space is not checked.
func TestMigratePushChecksTheDatabasesSpace(t *testing.T) {
	dbSpace := map[string]string{"mdb-dev": "space-dev", "mdb-main": "space-main", "mdb-old": ""}
	var applied []string
	dir := migrateRepo(t, migrateInstance(t, dbSpace, &applied))

	_, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-main")
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != syncReasonBindSpaceMismatch ||
		!strings.Contains(r.Detail, `stack "dev" is declared for Space Dev`) || !strings.Contains(r.Detail, "database mdb-main-name (mdb-main) is in Space Main") {
		t.Fatalf("push of Main's database on the dev stack: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "refused (bind_space_mismatch): ") || exitCodeOf(err) != 1 {
		t.Errorf("the mismatch does not lead with its reason code at exit 1: %v (exit %d)", err, exitCodeOf(err))
	}
	if len(applied) != 0 {
		t.Fatalf("applied anyway: %v", applied)
	}

	if _, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-dev"); err != nil {
		t.Fatalf("push of Dev's database on the dev stack: %v", err)
	}

	stderr := captureStderr(t, func() {
		if _, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-old"); err != nil {
			t.Errorf("push on an instance that names no Space: %v", err)
		}
	})
	if !strings.Contains(stderr, "does not say which Space database mdb-old is in, so it was not checked") {
		t.Errorf("no note for an absent spaceID:\n%s", stderr)
	}

	if _, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "main", "--database", "mdb-dev"); err != nil {
		t.Fatalf("push on a stack that declares no Space: %v", err)
	}
	if strings.Join(applied, ",") != "mdb-dev,mdb-old,mdb-dev" {
		t.Fatalf("applied %v", applied)
	}
}

// writeDevStackFolder writes a workflow folder at dir whose stack "dev" is on
// (url, tenantID) and declares spaceID.
func writeDevStackFolder(t *testing.T, dir, url, tenantID, spaceID string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{
  "formatVersion": 9,
  "kind": "workflow",
  "title": "Other",
  "entrypoint": "main.py",
  "stacks": {"dev": {"url": %q, "tenantID": %q, "featureID": "feat-x", "spaceID": %q}}
}
`, url, tenantID, spaceID)
	if err := os.WriteFile(filepath.Join(dir, wfdir.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Only stacks on this instance and organization are matched: a "dev" another
// organization declares does not refuse a database, and a credential that
// names no organization is matched once the instance has said which it is.
func TestMigratePushMatchesOnlyThisOrganizationsStacks(t *testing.T) {
	dbSpace := map[string]string{"mdb-dev": "space-dev", "mdb-main": "space-main", "mdb-old": ""}
	var applied []string
	url := migrateInstance(t, dbSpace, &applied)
	dir := migrateRepo(t, url)
	writeDevStackFolder(t, filepath.Join(filepath.Dir(dir), "workflows", "elsewhere"), url, "ten-other", "space-main")

	if _, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-dev"); err != nil {
		t.Fatalf("another organization's dev stack refused the push: %v", err)
	}

	if _, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-old"); err != nil {
		t.Fatalf("push recording mdb-old: %v", err)
	}
	dbSpace["mdb-old"] = "space-main"
	_, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev")
	if r, ok := asManagedRefusal(err); !ok || r.Reason != syncReasonBindSpaceMismatch {
		t.Fatalf("the remembered database in Main on a credential naming no organization: %v", err)
	}
	if strings.Join(applied, ",") != "mdb-dev,mdb-old" {
		t.Fatalf("applied %v", applied)
	}
}

// A folder whose ronja.json cannot be read could declare the stack's Space,
// so the push is refused at exit 2, naming it and the unchecked way out.
func TestMigratePushRefusesWhatItCannotRead(t *testing.T) {
	var applied []string
	url := migrateInstance(t, map[string]string{"mdb-dev": "space-dev"}, &applied)
	dir := migrateRepo(t, url)
	broken := filepath.Join(filepath.Dir(dir), "workflows", "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, wfdir.ManifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-dev")
	if err == nil || !strings.Contains(err.Error(), filepath.Join("broken", wfdir.ManifestName)) ||
		!strings.Contains(err.Error(), "--database <id> without --stack") || exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("push beside an unreadable ronja.json: %v (exit %d)", err, exitCodeOf(err))
	}
	if len(applied) != 0 {
		t.Fatalf("applied anyway: %v", applied)
	}
}

// A folder nested inside another is read like any other: its "dev" can
// declare a Space too, and the refusal of two Spaces names each one's folder.
func TestMigratePushReadsANestedFoldersStack(t *testing.T) {
	var applied []string
	url := migrateInstance(t, map[string]string{"mdb-dev": "space-dev"}, &applied)
	dir := migrateRepo(t, url)
	writeDevStackFolder(t, filepath.Join(filepath.Dir(dir), "workflows", "report", "inner"), url, testTenantID, "space-main")

	_, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-dev")
	if r, ok := asManagedRefusal(err); !ok || r.Reason != syncReasonBindSpaceMismatch ||
		!strings.Contains(r.Detail, "Space Dev (workflows/report) and Space space-main (workflows/report/inner)") {
		t.Fatalf("push with a nested folder declaring dev in Main: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("applied anyway: %v", applied)
	}
}

// Outside a git work tree no stack's Space can be read, so --stack is
// refused at exit 2, naming the unchecked way out, and nothing is applied.
func TestMigratePushWithAStackOutsideGitIsRefused(t *testing.T) {
	var applied []string
	migrateInstance(t, map[string]string{"mdb-dev": "space-dev"}, &applied)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "migrations", "0001_leads.sql"), []byte("CREATE TABLE leads (id int);\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-dev")
	if err == nil || !strings.Contains(err.Error(), "not inside a git work tree, so no stack's Space could be read") ||
		!strings.Contains(err.Error(), "--database <id> without --stack") || exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("--stack outside git: %v (exit %d)", err, exitCodeOf(err))
	}
	if len(applied) != 0 {
		t.Fatalf("applied anyway: %v", applied)
	}
	if _, err := runCLI(t, dir, "db", "migrate", "push", "--database", "mdb-dev"); err != nil {
		t.Fatalf("push with no --stack outside git: %v", err)
	}
}

// A database whose Space could not be read is no answer about it: the push is
// refused at exit 2, as "could not tell", and nothing is applied.
func TestMigratePushThatCannotReadTheDatabaseExitsTwo(t *testing.T) {
	var applied []string
	dir := migrateRepo(t, migrateInstance(t, map[string]string{"mdb-hidden": "forbidden"}, &applied))

	_, err := runCLI(t, dir, "db", "migrate", "push", "--stack", "dev", "--database", "mdb-hidden")
	if err == nil || !strings.Contains(err.Error(), "check which Space database mdb-hidden is in") || exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("push when the database cannot be read: %v (exit %d)", err, exitCodeOf(err))
	}
	if len(applied) != 0 {
		t.Fatalf("applied anyway: %v", applied)
	}
}
