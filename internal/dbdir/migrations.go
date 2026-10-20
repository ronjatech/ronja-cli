package dbdir

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// ReadMigrations loads migrations/*.sql from dir. A migration's name is its
// filename stem, and order is lexical by filename (10_x sorts before 9_x)
// deliberately: no natural sort, since re-ordering after the fact is drift.
//
// The SQL is read verbatim: the server's ledger hash covers exactly these
// bytes, and any tidying would report every applied migration as drifted.
func ReadMigrations(dir string) ([]api.Migration, error) {
	root := filepath.Join(dir, MigrationsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %s/ folder here — create one and put your .sql files in it", MigrationsDir)
		}
		return nil, fmt.Errorf("read %s: %w", root, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	out := make([]api.Migration, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		// Stems equal up to case would put two migrations under one ledger name
		// on a case-insensitive filesystem, making drift detection ambiguous.
		if prev, dup := seen[strings.ToLower(stem)]; dup {
			return nil, fmt.Errorf("%s and %s would both be the migration %q — rename one", prev, name, stem)
		}
		seen[strings.ToLower(stem)] = name

		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filepath.Join(MigrationsDir, name), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("%s is empty — a migration with no SQL has nothing to apply", filepath.Join(MigrationsDir, name))
		}
		out = append(out, api.Migration{Name: stem, SQL: string(body)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no .sql files in %s/", MigrationsDir)
	}
	return out, nil
}
