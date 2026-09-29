package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/checkfile"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The pipeline loop's HEALTH-CHECK half: `checks/<stem>.json`, the assertions a
// folder makes about the tables it builds.
//
// Checks are UNGOVERNED — no draft, no review, no version history — and they
// attach to a LIVE table id (a check put on a draft would stay on the version
// snapshot the commit leaves behind). So this pass never writes against a draft,
// and it writes against the live table only when THE LIVE-MATCHES RULE holds:
// the live row's SQL equals the file, and the caller has no open draft of it.
// Otherwise the file's checks are `checks_pending_publish` and nothing is sent;
// `publish` applies them right after the commit lands, and the next `push`
// finds live == file and lands them if the publish went to review instead.
//
// It visits EVERY stem with a checks file, not only the .sql files a push found
// changed: the main case is checks added to a table whose SQL did not move. A
// bare push also visits every checks file the lock records and the folder no
// longer holds — a file with no entries, so the checks it created are orphans
// (withDeletedChecksFiles).
//
// ONE TABLE'S CHECKS ARE APPLIED IN AN ORDER THAT IS LOAD-BEARING:
//
//  1. read the table's checks once;
//  2. refuse on DRIFT (a check this folder owns was changed in the app since
//     the folder last verified it) before anything is written, unless --force;
//  3. ORPHANS — owned ROWS no entry resolves to (planTableChecks: each entry's
//     row is found first, so a check renamed in the app is not an orphan of a
//     file that renamed it too): refused unless --prune, and with
//     --prune silenced BEFORE any create — a renamed freshness watchdog, or a
//     rename at the per-table cap, only succeeds in one push that way, because
//     the server allows one enabled watchdog and counts enabled checks;
//  4. updates (only the declared fields that differ), then creates;
//  5. one more read, and each written check is recorded only if the live row
//     now says what the file says. An older server's PUT accepts
//     {"enabled", "expression"} with a 200 and drops everything but `enabled`,
//     and this is what catches it.

// Health-check outcomes. --json values, so a contract; the exit status is
// non-zero iff any of them is one checksOutcomeFails names.
const (
	checksOutcomeApplied        = "applied"
	checksOutcomeUnchanged      = "unchanged"
	checksOutcomeAdopted        = "adopted"
	checksOutcomeAdoptedChanged = "adopted_changed"
	checksOutcomeSilenced       = "silenced"
	checksOutcomePendingPublish = "checks_pending_publish"
	checksOutcomeDrift          = "drift"
	checksOutcomeOrphaned       = "orphaned"
	checksOutcomeRefused        = "refused"
	checksOutcomeNotApplied     = "not_applied"
	checksOutcomeUnsupported    = "unsupported"
)

func checksOutcomeFails(outcome string) bool {
	switch outcome {
	case checksOutcomeDrift, checksOutcomeOrphaned, checksOutcomeRefused,
		checksOutcomeNotApplied, checksOutcomeUnsupported:
		return true
	}
	return false
}

// checksOutcomeSettled is an outcome that leaves nothing for a later push to do.
func checksOutcomeSettled(outcome string) bool {
	switch outcome {
	case checksOutcomeUnchanged, checksOutcomeAdopted, checksOutcomeSilenced:
		return true
	}
	return false
}

// pipelineChecksFile is one committed checks file, resolved as far as the folder
// can resolve it without the network.
type pipelineChecksFile struct {
	Path string
	// SQLPath is the .sql file the stem names.
	SQLPath string
	File    checkfile.File
	// Problem is why this file cannot be used as it stands; reported per file.
	Problem string
	// Deleted is a checks file the lock (or baseline) has a record for and the
	// folder no longer holds: a file with no entries, on the table it was
	// recorded against (TableID), so every check it created that is still
	// enabled is an orphan. See withDeletedChecksFiles.
	Deleted bool
	TableID string
}

// withDeletedChecksFiles appends a Deleted file for each recorded checks path
// the folder no longer holds, after the files it does hold — last, so a file a
// rename moved the checks into has recorded them by the time the old record is
// read (see checksClaimFor).
func withDeletedChecksFiles(files []pipelineChecksFile, store liveHashes) []pipelineChecksFile {
	present := map[string]bool{}
	for _, file := range files {
		present[file.Path] = true
	}
	for _, path := range store.checksPaths() {
		if present[path] {
			continue
		}
		tableID, _ := store.checksSeen(path)
		if tableID == "" {
			continue
		}
		files = append(files, pipelineChecksFile{Path: path, Deleted: true, TableID: tableID})
	}
	return files
}

// checksClaim is what the folder's OTHER checks files claim on one table: the
// names they declare and the checks they own. A deleted file's record never
// orphans a check another file on the same table claims — renaming a table's
// stem together with its checks file (and rebinding it) hands the checks over,
// and pruning them as the old file's orphans would silence what the new file
// declares.
type checksClaim struct {
	folds map[string]bool
	ids   map[string]bool
}

func (c checksClaim) has(row *api.TableCheck) bool {
	return row != nil && (c.ids[row.ID] || c.folds[checkfile.Fold(row.Name)])
}

// checksClaimFor is the claim on tableID of every present checks file other
// than `except`: what their entries declare (by the binding, whether or not
// their table is ready), and what the store records them as owning there.
//
// A file with a Problem still claims what the store records it as owning — the
// lock, not the unreadable file, says that. Only its declared names are left
// out, since they could not be read.
func checksClaimFor(f *folder, files []pipelineChecksFile, store liveHashes, tableID, except string) checksClaim {
	claim := checksClaim{folds: map[string]bool{}, ids: map[string]bool{}}
	for _, file := range files {
		if file.Deleted || file.Path == except {
			continue
		}
		if file.Problem == "" && f.Binding.Tables[file.SQLPath] == tableID {
			for _, e := range file.File.Checks {
				claim.folds[checkfile.Fold(e.Name)] = true
			}
		}
		if recordedTable, owned := store.checksSeen(file.Path); recordedTable == tableID {
			for _, m := range owned {
				claim.ids[m.CheckID] = true
			}
		}
	}
	return claim
}

// readChecksFiles reads a pipeline folder's `checks/` directory. A missing
// directory is the ordinary case. Flat, one level, like `metrics/`.
func readChecksFiles(root string, local map[string]string) ([]pipelineChecksFile, error) {
	dir := filepath.Join(root, wfdir.ChecksDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	sqlPaths := sortedPaths(local)
	var out []pipelineChecksFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if hasJSONBeneath(filepath.Join(dir, name)) {
				out = append(out, pipelineChecksFile{
					Path:    wfdir.ChecksDirName + "/" + name,
					Problem: fmt.Sprintf("%s/%s/ is a directory, and checks are one flat file per table (%s)", wfdir.ChecksDirName, name, wfdir.ChecksPath("<stem>")),
				})
			}
			continue
		}
		path := wfdir.ChecksDirName + "/" + name
		stem, ok := wfdir.ChecksStem(path)
		if !ok {
			continue
		}
		file := pipelineChecksFile{Path: path}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			file.Problem = fmt.Sprintf("could not be read: %v", err)
			out = append(out, file)
			continue
		}
		parsed, err := checkfile.Parse(body)
		if err != nil {
			file.Problem = err.Error()
			out = append(out, file)
			continue
		}
		file.File = parsed
		if file.SQLPath, err = checkfile.ResolveStem(stem, sqlPaths); err != nil {
			file.Problem = err.Error()
		}
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// splitChecksArgs takes the checks files out of a push's positional arguments,
// before the docs/metrics/SQL split sees them. Naming a path narrows every pass,
// so with arguments the checks pass covers the files named, plus the checks of
// every .sql file named.
func splitChecksArgs(root string, args []string, files []pipelineChecksFile) (rest []string, selected []pipelineChecksFile, err error) {
	byPath := make(map[string]pipelineChecksFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	for _, arg := range args {
		abs, absErr := filepath.Abs(arg)
		if absErr != nil {
			return nil, nil, fmt.Errorf("resolve %s: %w", arg, absErr)
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return nil, nil, fmt.Errorf("resolve %s against %s: %w", arg, root, relErr)
		}
		rel = filepath.ToSlash(rel)
		if file, ok := byPath[rel]; ok {
			selected = append(selected, file)
			continue
		}
		if _, isChecksPath := wfdir.ChecksStem(rel); isChecksPath {
			return nil, nil, fmt.Errorf("%s is not a checks file in this folder", arg)
		}
		rest = append(rest, arg)
	}
	return rest, selected, nil
}

// checksCloneFile is one table's checks as clone lays them down.
type checksCloneFile struct {
	Path    string
	TableID string
	Body    []byte
	Managed map[string]wfdir.LockCheckEntry
	Count   int
}

// fetchCloneChecks reads each cloned table's checks and renders the ENABLED
// ones as `checks/<stem>.json`: name, body, severity and a non-empty
// description, never `enabled` — silencing is the operator's, not the file's.
// Silenced checks are left out (they read as unmanaged), which also keeps the
// file inside the parser's one-watchdog rule when a renamed watchdog left a
// silenced one behind. A watchdog whose interval is not whole hours is skipped
// with a note: hours is the only unit the file speaks.
//
// A table whose checks cannot be read is skipped with a note rather than
// failing the clone — the SQL is what a clone is for.
func fetchCloneChecks(ctx context.Context, client *api.Client, sources []cloneSource) []checksCloneFile {
	var out []checksCloneFile
	for _, s := range sources {
		rows, err := client.ListTableChecks(ctx, s.TableID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not read the health checks on %s (%v) — %s was not written.\n",
				s.TableID, err, wfdir.ChecksPath(tableNameFor(s.Path)))
			continue
		}
		var file checkfile.File
		var kept []api.TableCheck
		for _, row := range rows {
			if !row.Enabled {
				continue
			}
			entry := checkfile.Entry{Name: row.Name}
			if row.Kind == api.TableCheckKindFreshness {
				if row.ExpectedIntervalSeconds <= 0 || row.ExpectedIntervalSeconds%3600 != 0 {
					fmt.Fprintf(os.Stderr, "  Note: %q on %s expects a rebuild every %ds, which is not a whole number of hours — left out of %s; it stays on the table, unmanaged.\n",
						row.Name, s.Path, row.ExpectedIntervalSeconds, wfdir.ChecksPath(tableNameFor(s.Path)))
					continue
				}
				entry.ExpectedIntervalHours = row.ExpectedIntervalSeconds / 3600
			} else {
				entry.Expression = strings.TrimSpace(row.Expression)
			}
			severity := row.Severity
			entry.Severity = &severity
			if desc := strings.TrimSpace(row.Description); desc != "" {
				entry.Description = &desc
			}
			file.Checks = append(file.Checks, entry)
			kept = append(kept, row)
		}
		if len(file.Checks) == 0 {
			continue
		}
		body, err := checkfile.Encode(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not render the checks on %s (%v).\n", s.TableID, err)
			continue
		}
		// Re-parsed, so a clone never writes a file its own push would refuse
		// (two names the server's collation keeps apart but lower() folds).
		if _, err := checkfile.Parse(body); err != nil {
			fmt.Fprintf(os.Stderr, "  Note: the checks on %s cannot be written as a checks file (%v) — left out.\n", s.TableID, err)
			continue
		}
		managed := map[string]wfdir.LockCheckEntry{}
		for i, entry := range file.Checks {
			managed[checkfile.Fold(entry.Name)] = wfdir.LockCheckEntry{CheckID: kept[i].ID, LiveSHA256: entry.LiveSHA256(liveCheckOf(&kept[i]))}
		}
		out = append(out, checksCloneFile{
			Path: wfdir.ChecksPath(tableNameFor(s.Path)), TableID: s.TableID,
			Body: body, Managed: managed, Count: len(file.Checks),
		})
	}
	return out
}
