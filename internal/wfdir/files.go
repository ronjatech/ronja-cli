package wfdir

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The server's path limits, mirrored from validatePath in
// backend/resource/rworkflow/store_files.go and backend/resource/rdataapp/store_files.go
// so a rejected file is reported before a push. They must stay in step: looser
// here means a mid-push 400, stricter means silently skipping a file the user
// expected to push.
const (
	MaxPathLength = 256
	MaxPathDepth  = 8
)

// ValidatePath reports whether a relative path is one the server will accept:
// slash-separated, at most MaxPathDepth segments, at most MaxPathLength
// characters, each segment drawn from [A-Za-z0-9_.-] and non-empty, no leading
// or trailing slash, no traversal.
func ValidatePath(path string) error {
	if path == "" {
		return fmt.Errorf("path must not be empty")
	}
	if len(path) > MaxPathLength {
		return fmt.Errorf("path exceeds %d characters", MaxPathLength)
	}
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("path must not contain null bytes")
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("path must be relative (no leading slash)")
	}
	if strings.HasSuffix(path, "/") {
		return fmt.Errorf("path must not end with a slash")
	}
	if strings.Contains(path, "//") {
		return fmt.Errorf("path must not contain consecutive slashes")
	}
	lower := strings.ToLower(path)
	if strings.Contains(lower, "..") || strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") {
		return fmt.Errorf("path must not contain traversal sequences")
	}
	segments := strings.Split(path, "/")
	if len(segments) > MaxPathDepth {
		return fmt.Errorf("path depth exceeds %d segments", MaxPathDepth)
	}
	for _, seg := range segments {
		if seg == "" {
			return fmt.Errorf("path must not contain empty segments")
		}
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= 'A' && r <= 'Z':
			case r >= '0' && r <= '9':
			case r == '_' || r == '-' || r == '.':
			default:
				return fmt.Errorf("path contains disallowed character %q", r)
			}
		}
	}
	return nil
}

// StructuralExclusion reports why a resource-relative path can never live in a
// synced folder of this kind, or "" when it can.
//
// The server's path grammar accepts every path this excludes (`.env`,
// `dist/app.js`), so anything that lays files out and then trusts a baseline
// must refuse them: the file would be written, never enumerated again, and read
// as a local deletion that the next push acts on. Kind is a parameter because
// the answer differs per kind, and this must agree with Enumerate.
func StructuralExclusion(path string, kind Kind) string {
	if path == ManifestName {
		return fmt.Sprintf("%s is the folder's own manifest", ManifestName)
	}
	if path == LockName {
		// The lock is committed, so it sits in the walk; a workflow or data app
		// has no SyncExt, and without this line the lock would be PUT into the
		// customer's resource and a later clone would write the server's stale
		// copy over the real one.
		return fmt.Sprintf("%s is the folder's own recorded state", LockName)
	}
	if path == StateDirName || strings.HasPrefix(path, StateDirName+"/") {
		return fmt.Sprintf("%s/ is the local-only sync baseline", StateDirName)
	}
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, ".") {
			return fmt.Sprintf("%q is a dot-name, and dot-files and dot-directories are never synced", seg)
		}
		for _, skip := range kind.SkipDirs {
			if seg == skip {
				return fmt.Sprintf("%s/ is dependency or build output, which a %s never holds", skip, kind.Label)
			}
		}
	}
	return ""
}

// Syncable reports whether a FILE path holds content a folder of this kind
// sends. Case-insensitive, because `.SQL` is an ordinary thing to find in a repo
// that has been through Windows.
func (k Kind) Syncable(path string) bool {
	return k.SyncExt == "" || strings.EqualFold(filepath.Ext(path), k.SyncExt)
}

// NotSyncable reports why a FILE path is not one this kind syncs, or "" when it
// is: StructuralExclusion plus the kind's SyncExt.
//
// Enumerate and CheckLocalPaths both apply it and must answer identically: a
// file one keeps and the other drops becomes a phantom deletion the next push
// performs server-side. FILES only: a directory is tested by StructuralExclusion
// alone, since `staging/` has no extension and pruning it would hide its .sql files.
func NotSyncable(path string, kind Kind) string {
	if reason := StructuralExclusion(path, kind); reason != "" {
		return reason
	}
	if !kind.Syncable(path) {
		return fmt.Sprintf("a %s folder syncs %s files and ignores everything else", kind.Label, kind.SyncExt)
	}
	return ""
}

// isFolderMachinery reports whether a path is the CLI's own bookkeeping, which
// is excluded from Skipped reporting. It must list exactly what
// StructuralExclusion rules out as machinery, or a file is either pushed into
// the customer's resource or named as their mistake on every command.
func isFolderMachinery(path string) bool {
	return path == ManifestName || path == LockName ||
		path == StateDirName || strings.HasPrefix(path, StateDirName+"/")
}

// Skipped is a local file that will never be synced, and why.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Enumeration is the syncable content of a folder plus what was left out.
type Enumeration struct {
	// Files maps a syncable relative path to its sha256.
	Files map[string]string
	// Skipped lists everything the walk left out, sorted by path, except the
	// folder's own machinery. An excluded directory is reported once, not per
	// file, so .git does not drown the report.
	Skipped []Skipped
}

// Enumerate walks a synced folder and hashes every file that could be pushed.
// Every exclusion except the folder's own machinery is reported as Skipped.
// Symlinks are not followed, so a link pointing outside the folder cannot
// smuggle content into a push.
func Enumerate(root string, kind Kind) (*Enumeration, error) {
	out := &Enumeration{Files: map[string]string{}}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if reason := StructuralExclusion(rel, kind); reason != "" {
				if !isFolderMachinery(rel) {
					out.Skipped = append(out.Skipped, Skipped{Path: rel + "/", Reason: reason})
				}
				return fs.SkipDir
			}
			return nil
		}
		// Name-based exclusions first, so a symlinked .env is reported by name
		// and a pipeline folder's venv/ is ruled out by extension before any read.
		if reason := NotSyncable(rel, kind); reason != "" {
			if !isFolderMachinery(rel) {
				out.Skipped = append(out.Skipped, Skipped{Path: rel, Reason: reason})
			}
			return nil
		}
		if !d.Type().IsRegular() {
			out.Skipped = append(out.Skipped, Skipped{Path: rel, Reason: "not a regular file"})
			return nil
		}
		if err := ValidatePath(rel); err != nil {
			out.Skipped = append(out.Skipped, Skipped{Path: rel, Reason: err.Error()})
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		out.Files[rel] = Hash(body)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out.Skipped, func(i, j int) bool { return out.Skipped[i].Path < out.Skipped[j].Path })
	return out, nil
}

// Diff is a comparison of two path → sha256 maps: what the left side has that
// the right side does not, and so on. Lists are sorted so output is stable.
type Diff struct {
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Deleted  []string `json:"deleted"`
	// Unchanged is a count rather than a list: it is the boring majority, and
	// nothing acts on the names.
	Unchanged int `json:"unchanged"`
}

// Dirty reports whether anything differs at all.
func (d Diff) Dirty() bool {
	return len(d.Added) > 0 || len(d.Modified) > 0 || len(d.Deleted) > 0
}

// Total is how many paths differ.
func (d Diff) Total() int { return len(d.Added) + len(d.Modified) + len(d.Deleted) }

// DiffHashes compares a current set of hashes against a baseline; the sync loop
// uses it for both local changes and remote drift.
func DiffHashes(current, baseline map[string]string) Diff {
	d := Diff{Added: []string{}, Modified: []string{}, Deleted: []string{}}
	for path, hash := range current {
		base, ok := baseline[path]
		switch {
		case !ok:
			d.Added = append(d.Added, path)
		case base != hash:
			d.Modified = append(d.Modified, path)
		default:
			d.Unchanged++
		}
	}
	for path := range baseline {
		if _, ok := current[path]; !ok {
			d.Deleted = append(d.Deleted, path)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Modified)
	sort.Strings(d.Deleted)
	return d
}

// WriteFile writes one workflow file into the folder, creating parent
// directories as needed. The path comes from the server and is validated first,
// so a row carrying a traversal path cannot write outside the folder.
func WriteFile(root, path, content string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(full), err)
	}
	return writeAtomic(full, []byte(content), 0o644)
}

// CheckLocalPaths reports whether a REMOTE file set can be laid out in a folder
// without losing a file, and must run before the first byte is written. A file
// the baseline records but the walk never returns is read as a local deletion
// that the next push performs server-side, so it refuses paths WriteFile
// rejects, paths NotSyncable excludes (it must agree with Enumerate), and paths
// differing only by case.
//
// The case check is unconditional rather than a filesystem probe, so a folder
// cloned on Linux stays usable on a case-insensitive Mac or Windows checkout.
func CheckLocalPaths(paths []string, kind Kind) error {
	var invalid []string
	for _, p := range paths {
		if err := ValidatePath(p); err != nil {
			invalid = append(invalid, fmt.Sprintf("%s (%v)", p, err))
		}
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return fmt.Errorf("this %s has %d file(s) whose paths cannot be written to disk:\n    %s\n  Rename them in the web builder, then clone again",
			kind.Label, len(invalid), strings.Join(invalid, "\n    "))
	}

	var excluded []string
	for _, p := range paths {
		if reason := NotSyncable(p, kind); reason != "" {
			excluded = append(excluded, fmt.Sprintf("%s (%s)", p, reason))
		}
	}
	if len(excluded) > 0 {
		sort.Strings(excluded)
		return fmt.Errorf("this %s has %d file(s) whose paths a synced folder cannot hold:\n    %s\n  They would be written once and then invisible to every later command — status would report them as deleted, and a push would delete them on the server.\n  Rename them in the web builder, then clone again",
			kind.Label, len(excluded), strings.Join(excluded, "\n    "))
	}

	// Grouped by distinct path, so a path returned twice does not collide with itself.
	byFold := map[string]map[string]bool{}
	for _, p := range paths {
		fold := strings.ToLower(p)
		if byFold[fold] == nil {
			byFold[fold] = map[string]bool{}
		}
		byFold[fold][p] = true
	}
	var collisions []string
	for _, group := range byFold {
		if len(group) < 2 {
			continue
		}
		distinct := make([]string, 0, len(group))
		for p := range group {
			distinct = append(distinct, p)
		}
		sort.Strings(distinct)
		collisions = append(collisions, strings.Join(distinct, " and "))
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		return fmt.Errorf("this %s has file paths that differ only by capitalisation, which are one file on a case-insensitive filesystem:\n    %s\n  Rename them in the web builder, then clone again",
			kind.Label, strings.Join(collisions, "\n    "))
	}
	return nil
}

// DirIsEmpty reports whether a directory has no entries. A missing directory
// counts as empty — the caller is about to create it.
func DirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	return len(entries) == 0, nil
}
