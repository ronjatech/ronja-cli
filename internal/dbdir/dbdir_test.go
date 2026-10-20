package dbdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

func writeMigration(t *testing.T, dir, name, body string) {
	t.Helper()
	root := filepath.Join(dir, MigrationsDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The server's ledger hash covers exactly the bytes sent; trimming even the
// trailing newline would report every applied migration as drifted.
func TestReadMigrationsIsVerbatim(t *testing.T) {
	dir := t.TempDir()
	const body = "\nCREATE TABLE leads (id int);\n\n"
	writeMigration(t, dir, "0001_leads.sql", body)

	got, err := ReadMigrations(dir)
	if err != nil {
		t.Fatalf("ReadMigrations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d migrations, want 1", len(got))
	}
	if got[0].SQL != body {
		t.Fatalf("SQL = %q, want the file's bytes verbatim %q", got[0].SQL, body)
	}
}

func TestReadMigrationsNamesFromStem(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "0001_leads.sql", "CREATE TABLE leads (id int);")

	got, err := ReadMigrations(dir)
	if err != nil {
		t.Fatalf("ReadMigrations: %v", err)
	}
	if got[0].Name != "0001_leads" {
		t.Fatalf("Name = %q, want the filename stem %q", got[0].Name, "0001_leads")
	}
}

// 10_ sorts before 9_ on purpose: a natural sort would disagree with the order
// an earlier run recorded in the ledger.
func TestReadMigrationsOrdersLexically(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"9_c.sql", "10_b.sql", "0001_a.sql"} {
		writeMigration(t, dir, n, "SELECT 1;")
	}

	got, err := ReadMigrations(dir)
	if err != nil {
		t.Fatalf("ReadMigrations: %v", err)
	}
	want := []string{"0001_a", "10_b", "9_c"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("position %d is %q, want %q (order is lexical: %v)", i, got[i].Name, w, want)
		}
	}
}

func TestReadMigrationsIgnoresNonSQL(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "0001_leads.sql", "CREATE TABLE leads (id int);")
	writeMigration(t, dir, "README.md", "notes")
	writeMigration(t, dir, "0001_leads.sql.bak", "CREATE TABLE old (id int);")

	got, err := ReadMigrations(dir)
	if err != nil {
		t.Fatalf("ReadMigrations: %v", err)
	}
	if len(got) != 1 || got[0].Name != "0001_leads" {
		t.Fatalf("got %+v, want just the .sql file", got)
	}
}

func TestReadMigrationsRefusesDuplicateStem(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "0001_leads.sql", "CREATE TABLE a (i int);")
	writeMigration(t, dir, "0001_leads.SQL", "CREATE TABLE b (i int);")

	_, err := ReadMigrations(dir)
	// On a case-insensitive filesystem the second write replaces the first, so
	// success is legitimate there; two migrations sharing a name is not.
	if err == nil {
		got, rerr := ReadMigrations(dir)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(got) != 1 {
			t.Fatalf("got %d migrations sharing a stem; want a refusal or a single file", len(got))
		}
		return
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Fatalf("refusal should tell the user to rename one, got: %v", err)
	}
}

// An empty set sent to the server succeeds as a no-op, so a typo'd --dir would
// read as "already up to date".
func TestReadMigrationsRefusesEmpty(t *testing.T) {
	t.Run("no folder", func(t *testing.T) {
		if _, err := ReadMigrations(t.TempDir()); err == nil {
			t.Fatal("want an error when there is no migrations/ folder")
		}
	})
	t.Run("no sql files", func(t *testing.T) {
		dir := t.TempDir()
		writeMigration(t, dir, "README.md", "notes")
		if _, err := ReadMigrations(dir); err == nil {
			t.Fatal("want an error when the folder holds no .sql files")
		}
	})
	t.Run("empty migration", func(t *testing.T) {
		dir := t.TempDir()
		writeMigration(t, dir, "0001_blank.sql", "   \n")
		if _, err := ReadMigrations(dir); err == nil {
			t.Fatal("want an error for a migration with no SQL")
		}
	})
}

// =============================================================================
// Binding
// =============================================================================

func TestBindingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "tnt-1"}

	b, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Save(dir, key, "mdb-abc"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.DatabaseFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if got != "mdb-abc" {
		t.Fatalf("DatabaseFor = %q, want mdb-abc", got)
	}
}

func TestBindingMissingFileIsEmpty(t *testing.T) {
	b, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load on a fresh folder: %v", err)
	}
	got, err := b.DatabaseFor(wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "tnt-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("DatabaseFor = %q on an unbound folder, want empty", got)
	}
}

// Keyed by URL alone, the second Save would overwrite the first, and the next
// push would send one organization's database id under the other's token.
func TestBindingIsPerOrganization(t *testing.T) {
	dir := t.TempDir()
	first := wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "tnt-1"}
	second := wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "tnt-2"}

	b, _ := Load(dir)
	if err := b.Save(dir, first, "mdb-one"); err != nil {
		t.Fatal(err)
	}
	b, _ = Load(dir)
	if err := b.Save(dir, second, "mdb-two"); err != nil {
		t.Fatal(err)
	}

	reloaded, _ := Load(dir)
	gotFirst, err := reloaded.DatabaseFor(first)
	if err != nil {
		t.Fatal(err)
	}
	gotSecond, err := reloaded.DatabaseFor(second)
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst != "mdb-one" {
		t.Fatalf("first organization resolves to %q, want mdb-one — the second Save overwrote it", gotFirst)
	}
	if gotSecond != "mdb-two" {
		t.Fatalf("second organization resolves to %q, want mdb-two", gotSecond)
	}
}

func TestBindingResaveReplaces(t *testing.T) {
	dir := t.TempDir()
	key := wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "tnt-1"}

	b, _ := Load(dir)
	if err := b.Save(dir, key, "mdb-old"); err != nil {
		t.Fatal(err)
	}
	b, _ = Load(dir)
	if err := b.Save(dir, key, "mdb-new"); err != nil {
		t.Fatal(err)
	}

	reloaded, _ := Load(dir)
	if len(reloaded.Instances) != 1 {
		t.Fatalf("got %d entries, want 1 (a re-save must replace, not append)", len(reloaded.Instances))
	}
	got, _ := reloaded.DatabaseFor(key)
	if got != "mdb-new" {
		t.Fatalf("DatabaseFor = %q, want mdb-new", got)
	}
}

func TestBindingAmbiguousWhenSignedOut(t *testing.T) {
	dir := t.TempDir()
	b, _ := Load(dir)
	_ = b.Save(dir, wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "tnt-1"}, "mdb-one")
	b, _ = Load(dir)
	_ = b.Save(dir, wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "tnt-2"}, "mdb-two")

	reloaded, _ := Load(dir)
	_, err := reloaded.DatabaseFor(wfdir.InstanceKey{URL: "https://acme.ronja.tech"})
	if !errors.Is(err, wfdir.ErrAmbiguousInstance) {
		t.Fatalf("err = %v, want ErrAmbiguousInstance", err)
	}
}

func TestBindingFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	b, _ := Load(dir)
	if err := b.Save(dir, wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "t"}, "mdb-abc"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

// The binding can be the first file in .ronja/, so it cannot assume the
// workflow commands already wrote the ignore.
func TestBindingWritesGitignore(t *testing.T) {
	dir := t.TempDir()
	b, _ := Load(dir)
	if err := b.Save(dir, wfdir.InstanceKey{URL: "https://acme.ronja.tech", TenantID: "t"}, "mdb-abc"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, DirName, ".gitignore"))
	if err != nil {
		t.Fatalf("read .ronja/.gitignore: %v", err)
	}
	if strings.TrimSpace(string(got)) != "*" {
		t.Fatalf(".gitignore = %q, want \"*\"", string(got))
	}
}
