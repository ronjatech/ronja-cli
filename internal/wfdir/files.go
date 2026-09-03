package wfdir

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The server's own limits, mirrored from backend/resource/rworkflow/
// store_files.go (validatePath) — and identical in backend/resource/rdataapp/
// store_files.go, whose validatePath is a deliberate copy of the same rules, so
// ONE grammar covers both kinds. They are duplicated rather than discovered
// because the point is to tell the caller a file will be rejected BEFORE a
// push spends a round-trip finding out — and to know, locally, which files in
// a folder are even candidates for sync.
//
// Keeping them in step with the server is load-bearing; a rule that drifts
// looser here means a confusing mid-push 400, and one that drifts stricter
// means silently skipping a file the user expected to push.
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
// These are the rules Enumerate applies BY SHAPE, before the server's path
// grammar gets a look in — and the server's grammar accepts every one of them
// (`.` is an allowed segment character, so `.env` and `.config/x.py` are
// perfectly legal workflow file paths server-side; so is `dist/app.js`). That
// asymmetry is the whole reason this is a named, exported rule rather than an
// inline condition: anything that lays files out on disk and then trusts a
// baseline — clone, the post-publish baseline refresh — has to refuse a set
// containing one of these, because the file would be written, never enumerated
// again, and read as a LOCAL DELETION on the next status (and acted on by the
// next push).
//
// Kind is a parameter for exactly that reason. A data app excludes node_modules/
// and dist/ where a workflow does not, so "is this path syncable" has no
// kind-free answer — and answering it with one Kind here and another in
// Enumerate is the phantom-deletion bug above.
//
// Keep in step with Enumerate.
func StructuralExclusion(path string, kind Kind) string {
	if path == ManifestName {
		return fmt.Sprintf("%s is the folder's own manifest", ManifestName)
	}
	if path == LockName {
		// The lock is COMMITTED, so unlike .ronja/ it is a real file sitting in
		// the walk's way — and a workflow or data app has no SyncExt, so without
		// this line it is ordinary source and gets PUT into the customer's own
		// resource. Three things then go wrong at once: the folder's machine
		// state ships inside the workflow (and inside a data app's compiled
		// bundle); a later clone writes the server's stale copy over the real
		// lock file, which LoadLock then reads as authoritative — a clone
		// inheriting another environment's ids; and the cloned folder reports
		// `modified ronja.lock.json` for ever, because every push rewrites it.
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
//
// A kind with no SyncExt syncs everything the structural rules allow, which is
// how workflows and data apps behave.
func (k Kind) Syncable(path string) bool {
	return k.SyncExt == "" || strings.EqualFold(filepath.Ext(path), k.SyncExt)
}

// NotSyncable reports why a FILE path is not one this kind syncs, or "" when it
// is. It is StructuralExclusion plus the kind's own content rule (SyncExt).
//
// The one predicate both the walk and the layout check apply — Enumerate for
// every file it finds, CheckLocalPaths for every path it is about to write —
// and they must answer identically. A file one keeps and the other drops leaves
// a baseline claiming a path that no later walk returns: `status` invents a
// deletion and the next push acts on it by deleting the file server-side.
//
// FILES only. A DIRECTORY is tested by StructuralExclusion alone: `staging/`
// has no extension, and pruning it here would hide every .sql file beneath it.
func NotSyncable(path string, kind Kind) string {
	if reason := StructuralExclusion(path, kind); reason != "" {
		return reason
	}
	if !kind.Syncable(path) {
		return fmt.Sprintf("a %s folder syncs %s files and ignores everything else", kind.Label, kind.SyncExt)
	}
	return ""
}

// isFolderMachinery reports whether a path is the CLI's own bookkeeping rather
// than something the user authored. Excluded from Skipped reporting: the user
// did not put ronja.json, ronja.lock.json or .ronja/ there expecting them to
// sync, and naming them on every single command would be pure noise.
//
// It must list exactly what StructuralExclusion rules out as machinery — the
// two answer the same question, one saying "never syncable" and the other
// "and do not report it". A file in one and not the other is either pushed
// into the customer's resource or named as their mistake on every command.
func isFolderMachinery(path string) bool {
	return path == ManifestName || path == LockName ||
		path == StateDirName || strings.HasPrefix(path, StateDirName+"/")
}

// Skipped is a local file that will never be synced, and why.
//
// Reported rather than silently dropped: a file the user believes is part of
// the workflow and which the CLI quietly ignores is the worst kind of
// surprise, and it is invisible in every other output.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Enumeration is the syncable content of a folder plus what was left out.
type Enumeration struct {
	// Files maps a syncable relative path to its sha256.
	Files map[string]string
	// Skipped lists everything the walk left out, sorted by path: path-invalid
	// files, non-regular files, AND the structural exclusions (dot-files,
	// dot-directories). A dot-DIRECTORY is reported once, by directory, rather
	// than once per file inside it — .git would otherwise drown the report.
	//
	// The folder's own machinery (ronja.json, .ronja/) is the one thing NOT
	// listed: it is the CLI's bookkeeping, not the user's mistake.
	Skipped []Skipped
}

// Enumerate walks a synced folder and hashes every file that could be pushed.
//
// Three classes of exclusion, in order:
//
//  1. The folder's own machinery: ronja.json, ronja.lock.json and .ronja/.
//     Never syncable, and the only exclusion not reported as Skipped.
//  2. Structural exclusions: dotfiles and dot-directories anywhere in the tree —
//     .git above all, which is enormous, full of paths the server would reject,
//     and never source — plus this Kind's SkipDirs (node_modules/, dist/ and
//     build/ for a data app). Ruled out by DIRECTORY so a repo's whole object
//     store, or a whole dependency tree, is skipped rather than walked and
//     discarded file by file (and reported once, by directory).
//  3. Anything else the server's path rules reject, which is reported as
//     Skipped so the caller can say so.
//
// Every exclusion but the first is REPORTED. Silently dropping a file the
// server would have accepted is what turns a clone into a folder whose baseline
// lies: see StructuralExclusion and CheckLocalPaths.
//
// Symlinks are not followed (WalkDir does not follow them), so a link pointing
// outside the folder cannot smuggle content into a push.
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
		// Always slash-separated: the wire format is a resource file path, not
		// a local one, and this has to behave the same on Windows.
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			// Directories are tested by the SAME rule as files, so a Kind's
			// SkipDirs prunes the whole subtree here rather than rejecting its
			// contents one by one below — and reports once, by directory.
			if reason := StructuralExclusion(rel, kind); reason != "" {
				if !isFolderMachinery(rel) {
					out.Skipped = append(out.Skipped, Skipped{Path: rel + "/", Reason: reason})
				}
				return fs.SkipDir
			}
			return nil
		}
		// The name-based exclusions come out first, so a symlinked .env is ruled
		// out by NAME rather than reported as "not a regular file" — and so a
		// pipeline folder's node_modules/ or venv/ is ruled out by EXTENSION
		// before anything is read off disk, which is the whole cost of the walk.
		if reason := NotSyncable(rel, kind); reason != "" {
			if !isFolderMachinery(rel) {
				out.Skipped = append(out.Skipped, Skipped{Path: rel, Reason: reason})
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// A symlink, socket or device is not workflow source. REPORTED
			// rather than dropped: a symlinked module is a perfectly ordinary
			// thing to have in a repo, and one that vanishes from every push
			// with no output anywhere is exactly the surprise Skipped exists
			// for. (WalkDir does not follow links, so the target's content
			// cannot be smuggled in either way.)
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

// DiffHashes compares a current set of hashes against a baseline.
//
// One function serves both comparisons the sync loop needs, because they are
// the same question asked twice:
//
//   - LOCAL vs baseline — what have I changed since the last sync (what push
//     would send).
//   - REMOTE vs baseline — what has changed on the server since the last sync
//     (drift: someone else's web-builder edit to the same per-user draft), which
//     push must refuse to overwrite blindly.
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
// directories as needed. The path is validated first: it arrives from the
// server, and a row carrying a traversal path (legacy data, a future bug)
// must not be able to write outside the folder.
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
// without losing a file, and is meant to run before the first byte is written.
//
// It exists because the baseline is a promise: .ronja/state.json says "these
// paths are on disk with these hashes", and every later command believes it. A
// file the server has but the folder does not is read as a LOCAL DELETION —
// status shows a phantom deleted entry, and a push acts on it by deleting the
// file server-side. Losing the user's code to a folder that was wrong from the
// moment it was created is not a warning-level event.
//
// Three ways a set cannot be laid out:
//
//   - A path WriteFile refuses (legacy rows, or a malicious one). Skipping it
//     with a warning is what produced the phantom-deletion case above.
//   - A path Enumerate excludes by NAME (NotSyncable): a dot-file or
//     dot-directory ANYWHERE in it, the folder's own ronja.json /
//     ronja.lock.json / .ronja/, one
//     of this Kind's SkipDirs, or a file outside this Kind's SyncExt — a `.md`
//     handed to a pipeline folder is written once and then invisible to every
//     walk. The server's path grammar allows `.` in a segment, so
//     `.env` and `.config/x.py` are legal workflow paths that clone would
//     happily write — and `dist/app.js` is a legal data-app path — and that the
//     walk would then never see again. Exactly the phantom-deletion shape above,
//     just arrived at by naming rather than by refusal. This is why Kind is a
//     parameter here and not only on Enumerate: the two must answer identically.
//   - Paths differing only by case. "Main.py" and "main.py" are two distinct
//     rows server-side and ONE file on macOS or Windows, so the second write
//     silently destroys the first — and the baseline then claims both exist.
//     Checked unconditionally rather than by probing the filesystem, so a clone
//     behaves identically everywhere and a folder cloned on Linux does not
//     become unusable the moment a colleague pulls it on a Mac.
//
// The remedy is not something the CLI can perform, so the error names the
// offending paths and points at the web builder, which can rename them.
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

	// Grouped by DISTINCT path, so a server that somehow returned the same path
	// twice is not described as colliding with itself.
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
