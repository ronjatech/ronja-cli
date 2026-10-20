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

// Checks attach to the LIVE table id, so they are written only when the live
// SQL equals the file and the caller has no open draft (the live-matches rule).
// One table's checks apply in a load-bearing order: read once, refuse on drift,
// silence orphans before any create (one enabled watchdog, a per-table cap),
// update then create, then re-read and record only what the live row confirms.
// See cli/README.md → "Health checks".

// Health-check outcomes are --json values, so a contract.
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
	// Deleted is a recorded checks file the folder no longer holds: a file with
	// no entries on TableID, so every still-enabled check it created is an orphan.
	Deleted bool
	TableID string
}

// withDeletedChecksFiles appends Deleted files last, so a file a rename moved
// the checks into has recorded them before the old record is read (see
// checksClaimFor).
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

// checksClaim is what the folder's OTHER checks files claim on one table. A
// deleted file's record never orphans a check another file claims, so renaming
// a stem with its checks file hands the checks over rather than silencing them.
type checksClaim struct {
	folds map[string]bool
	ids   map[string]bool
}

func (c checksClaim) has(row *api.TableCheck) bool {
	return row != nil && (c.ids[row.ID] || c.folds[checkfile.Fold(row.Name)])
}

// checksClaimFor: a file with a Problem still claims what the store records it
// as owning; only its unreadable declared names are left out.
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

// splitChecksArgs takes the checks files out of a push's arguments before the
// docs/metrics/SQL split. Naming a path narrows every pass; the caller adds the
// checks of every .sql file named.
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

type checksCloneFile struct {
	Path    string
	TableID string
	Body    []byte
	Managed map[string]wfdir.LockCheckEntry
	Count   int
}

// fetchCloneChecks never writes `enabled` (silencing is the operator's) and
// leaves silenced checks out, which also keeps the file inside the one-watchdog
// rule. An unreadable table's checks are skipped with a note: the SQL is what a
// clone is for.
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
