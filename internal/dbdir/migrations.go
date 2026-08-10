package dbdir

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// ReadMigrations loads migrations/*.sql from dir, in filename order.
//
// The name of a migration is its filename STEM — 0001_leads.sql is the migration
// "0001_leads" — so the file you can see in a diff is the thing the ledger
// records, with no separate manifest to fall out of step with it.
//
// Order is lexical by filename, which is why the conventional zero-padded prefix
// matters: 10_x.sql sorts before 9_x.sql, and a set applied in the wrong order
// fails at the database rather than quietly doing the wrong thing. That is a
// footgun worth naming rather than hiding behind a natural sort, because the
// ledger records what ran and re-ordering after the fact is drift.
//
// The SQL is read VERBATIM — no trimming, no newline normalisation. The server's
// ledger hash covers exactly these bytes, so a client that tidied them would
// make the same file hash differently from the way it was applied, and every
// applied migration would report as drifted forever after.
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
		// Two files whose stems collide — 0001_leads.sql and 0001_leads.SQL on a
		// case-insensitive filesystem — would send two migrations under one ledger
		// name, which is precisely the duplicate-name state that makes drift
		// detection ambiguous. Refuse rather than pick one.
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
