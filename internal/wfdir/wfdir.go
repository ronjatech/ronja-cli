// Package wfdir owns the on-disk shape of a synced folder — a workflow's or a
// data app's: what is committed to the customer's git (ronja.json), what is
// purely local sync bookkeeping (.ronja/state.json), and which local files are
// candidates for sync at all.
//
// The two kinds share this whole model because they ARE the same shape: a
// feature-owned resource whose content is a set of source files under a
// per-user draft/commit lifecycle, with identical server-side path rules. What
// differs is carried by Kind, and by the two kind-specific manifest blocks
// (Parameters for a workflow, Access for a data app). The package name predates
// data apps and is left alone rather than churning every import.
//
// The split is the whole design:
//
//   - ronja.json describes the RESOURCE — its title, its entrypoint, and which
//     row it maps to on each instance. It is instance-plural on
//     purpose (one repo pushing to staging and to production is the normal CI
//     case, not an exotic one), and it belongs in version control.
//   - .ronja/state.json is this CHECKOUT's baseline: the hash of every file as
//     the server last had it. Drafts are per-user, so a colleague cloning the
//     repo must not inherit someone else's baseline — hence never committed,
//     and hence clone/init drop a .ronja/.gitignore that excludes the whole
//     directory rather than trusting anyone to add it.
//
// Nothing here talks to the network, and the only non-stdlib import is
// internal/config — for NormalizeURL alone, because the manifest's per-instance
// keys have to fold exactly the way the credential store's do or the same
// instance ends up spelled two ways. That keeps the folder model unit-testable
// without a server.
package wfdir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The folder's fixed filenames.
const (
	// ManifestName is the committed manifest at the folder root.
	ManifestName = "ronja.json"
	// StateDirName holds everything local-only.
	StateDirName = ".ronja"
	// StateFileName is the sync baseline inside StateDirName.
	StateFileName = "state.json"
	// GitignoreName is written inside StateDirName with "*" in it, so the
	// baseline cannot be committed by accident.
	GitignoreName = ".gitignore"
)

// The manifest kinds this CLI understands. An unknown value is refused rather
// than ignored: the folder types are different resources with different
// semantics, and a command that pushed one's files into the other would be
// actively destructive.
const (
	KindWorkflow = "workflow"
	KindDataApp  = "dataapp"
	// KindPipeline is a folder of derived-table SQL files. Unlike the other two
	// it binds MANY resources — one table per .sql file — which is why Binding
	// carries a Tables map and why the single-id helpers refuse this kind rather
	// than answering with a workflow's field.
	KindPipeline = "pipeline"
)

// Kind is the resource a synced folder describes, plus the rules that differ
// between them.
//
// A single VALUE rather than a handful of parameters, because these rules have
// to travel TOGETHER. If Enumerate decides a path is not syncable while
// CheckLocalPaths thinks it is, the folder ends up with a baseline claiming a
// file is on disk that the walk will never see again — `status` invents a
// deletion and the next push acts on it by deleting the file server-side. One
// Kind threaded through every entry point makes that mismatch unrepresentable,
// where "remember to pass the same exclusion list to both" would not.
type Kind struct {
	// Name is the manifest's `kind` value, and the default directory name when a
	// title slugifies to nothing.
	Name string
	// Label is how the kind is spoken about in messages: "workflow", "data app".
	Label string
	// Command is the CLI command group that drives folders of this kind —
	// "ronja wf", "ronja app", "ronja pipeline".
	//
	// It lives ON the Kind rather than in a lookup beside it because it was
	// previously spelled twice: once in this package (for LoadManifest's
	// wrong-kind refusal, which names the command that WOULD work) and once in
	// internal/commands (for "run this inside a folder created by …"). Both were
	// binary if/else on KindDataApp, so a third kind would have been routed to
	// `ronja wf` by BOTH of them — silently, in exactly the messages a lost
	// caller reads. Declared here, a new kind cannot be added without answering
	// the question.
	Command string
	// DefaultEntrypoint matches the server's own default for a new resource of
	// this kind. EMPTY is a legitimate value (a pipeline folder has no
	// entrypoint at all): LoadManifest falls back to it, so an empty default
	// simply leaves Manifest.Entrypoint empty.
	DefaultEntrypoint string
	// SkipDirs are directory names this kind never syncs, on top of the dot-name
	// rule every folder shares. Matched by SEGMENT at any depth.
	//
	// Empty for workflows: a Python folder has no conventional build output, and
	// excluding a directory nobody has would only be a rule to trip over. A data
	// app is a TSX folder, where node_modules/ and dist/ are ordinary — and where
	// their absence from this list is expensive, because the server caps an app
	// at 100 files and would reject the push late, by count, naming nothing
	// useful.
	SkipDirs []string
	// SyncExt, when non-empty, is the ONE file extension a folder of this kind
	// synchronises; every other file in it is ignored, silently, so the folder
	// can be an ordinary repo directory.
	//
	// It lives on the Kind — beside SkipDirs and for the same reason — because
	// this is a rule Enumerate and CheckLocalPaths must answer IDENTICALLY: a
	// walk that drops a path the baseline keeps produces a baseline claiming a
	// file no later walk will see, which status reports as a deletion and a push
	// acts on. Threaded as one value, that mismatch is unrepresentable. See
	// NotSyncable, which is where both sides read it.
	SyncExt string
}

var (
	// WorkflowKind is a Python workflow folder.
	WorkflowKind = Kind{
		Name:              KindWorkflow,
		Label:             "workflow",
		Command:           "ronja wf",
		DefaultEntrypoint: "main.py",
	}
	// DataAppKind is a data-app folder.
	//
	// The entrypoint is the server's fixed default AND its only value: a data
	// app's entrypoint is stamped at create and rdataapp.Patch carries no field
	// to change it, so unlike a workflow's this is not a preference the manifest
	// can move.
	DataAppKind = Kind{
		Name:              KindDataApp,
		Label:             "data app",
		Command:           "ronja app",
		DefaultEntrypoint: "App.tsx",
		SkipDirs:          []string{"node_modules", "dist", "build"},
	}
	// PipelineKind is a folder of derived-table SQL files.
	//
	// No entrypoint: the other two kinds have ONE file the server runs, while a
	// pipeline folder is a SET of tables with no distinguished member — so the
	// concept has nothing to name here. Empty is carried deliberately rather
	// than defaulted to something harmless, because "" is what LoadManifest
	// falls back to and what SaveManifest round-trips.
	//
	// No SkipDirs, because SyncExt already covers what they would: a folder that
	// syncs `.sql` and nothing else has no reason to name node_modules/ or
	// venv/ — their contents are ruled out by extension, before anything is read
	// off disk.
	PipelineKind = Kind{
		Name:    KindPipeline,
		Label:   "pipeline",
		Command: "ronja pipeline",
		SyncExt: ".sql",
	}
)

// The workflow runtime versions a folder may declare.
//
// RuntimeDefault is what a workflow gets when the create body says nothing, and
// its behaviour is frozen: v1 runs stay bit-identical. RuntimeDurable journals
// every @tools.step result, which is what makes a failed run resumable.
//
// Two values, listed rather than range-checked, because the CLI has to refuse a
// third: the manifest is committed, and a folder declaring a runtime the
// instance has never heard of would create a workflow whose runtime nobody can
// name — discovered at the first run, not at the push.
const (
	RuntimeDefault = 1
	RuntimeDurable = 2
)

// DefaultEntrypoint matches the server's own default for a new workflow.
//
// Retained as a package-level constant because it is what `wf init` writes and
// what a manifest missing an entrypoint falls back to; new code should prefer
// Kind.DefaultEntrypoint.
const DefaultEntrypoint = "main.py"

// ManifestPath and StatePath locate the two files inside a folder root.
//
// Both are pure joins, for naming a path in a message. Anything that WRITES
// inside StateDirName goes through StateDir instead.
func ManifestPath(root string) string { return filepath.Join(root, ManifestName) }
func StatePath(root string) string    { return filepath.Join(root, StateDirName, StateFileName) }

// StateDir locates the folder's local-only state directory — or a path inside
// it, when sub is given — and refuses one a symlink puts OUTSIDE root.
//
// Everything under .ronja/ is written, overwritten and (for `app test`'s
// artefacts) deleted by the CLI at paths the CLI itself chose, not ones anybody
// typed. A folder is an ordinary git checkout, so `.ronja` — or any component
// under it — can arrive as a COMMITTED SYMLINK, and following one hands whoever
// wrote that repository the ability to point this CLI's writes and its deletes
// at any path on the machine running it.
//
// The check resolves the longest EXISTING prefix of the path and asserts the
// result is still inside root. Resolving the LEAF alone is not enough, which is
// the bug this replaced: os.Lstat follows every component before the last, so a
// link at `.ronja` itself answered "not a symlink" for `.ronja/test` and the
// guard passed. Components that do not exist yet cannot be links; they are the
// caller's to create. Both sides are resolved, because root itself routinely
// arrives through one (/var → /private/var on macOS, and every t.TempDir).
func StateDir(root string, sub ...string) (string, error) {
	dir := filepath.Join(append([]string{root, StateDirName}, sub...)...)
	realRoot, err := resolveExistingPrefix(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", root, err)
	}
	real, err := resolveExistingPrefix(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	if !underPath(realRoot, real) {
		return "", fmt.Errorf("%s is reached through a symlink that leaves the folder — it resolves to %s, so writing there would put files, and delete files, somewhere else entirely.\n  Remove the symlink to continue", dir, real)
	}
	return dir, nil
}

// resolveExistingPrefix resolves path's symlinks as far as path exists, then
// re-attaches whatever is not there yet.
//
// filepath.EvalSymlinks fails outright on a path whose last components are
// missing, which is the ordinary case here — the state directory is often
// created by the very command asking about it.
func resolveExistingPrefix(path string) (string, error) {
	cur := filepath.Clean(path)
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		// Only a MISSING component is walked past. Anything else (a file where a
		// directory should be, a permission failure) is reported rather than
		// stepped over, since it means the answer is unknown rather than "not
		// there yet".
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// underPath reports whether path is root itself or something inside it.
func underPath(root, path string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Hash is the content fingerprint used everywhere in this package: sha256 over
// the raw bytes, hex-encoded.
//
// The remote side has no hash of its own (workflow_files carries only content
// and timestamps), so the CLI hashes the `content` string it receives with
// exactly this function. Local and remote fingerprints are therefore comparable
// by construction — as long as both sides hash BYTES, which is why this takes a
// []byte and the string form converts rather than the other way round.
func Hash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// HashString fingerprints content that arrived as a string (i.e. from the API).
func HashString(body string) string { return Hash([]byte(body)) }

// writeAtomic writes a file via a temp file in the same directory plus a
// rename, so an interrupted write cannot leave a half-written manifest or
// baseline that the next command refuses to parse.
//
// Unlike config.Save this deliberately does NOT fsync. The credential store
// fsyncs because a lost token is unrecoverable — it was displayed nowhere and
// exists only on that disk. Everything here is recoverable: the manifest is in
// the user's git, and a lost baseline is re-derived by the next clone or push.
// Paying for durability we do not need would only make every command slower.
func writeAtomic(path string, body []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// Slug turns a title into a directory name: lowercase, ASCII alphanumerics and
// dashes, no runs of dashes, no leading or trailing dash.
//
// Used only to pick a default clone directory, so a title made entirely of
// characters this drops (an emoji, a non-Latin script) must still produce
// something — fallback — rather than an empty path.
func Slug(title, fallback string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return fallback
	}
	// Directory names have their own limits, and a 400-character title is a
	// plausible thing for a human to write.
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		return fallback
	}
	return slug
}

// FileSlug turns a resource's NAME into a filename stem. It is Slug plus two
// rules: an underscore survives, and so does CASE.
//
// The two are deliberately separate functions rather than one with a flag,
// because they name different things and only one of them round-trips. Slug
// names a clone DIRECTORY — a local choice, made once, that nothing compares
// against later. FileSlug names a file inside a folder that lives in the
// customer's git, and that file's stem is also the table's display name, so the
// name goes out and has to come back the same way. Folding `_` into `-` broke
// exactly that loop: an author writing orders_clean.sql got orders-clean.sql
// back from `pipeline clone`, which is a second file for one table and a
// phantom deletion the moment either is pushed.
//
// Everything outside [a-z0-9_-] becomes a separator, and a literal `-` is
// treated as one too rather than as an allowed character — so runs collapse
// ("Sales / Revenue" and "sales---revenue" both give one dash) exactly as they
// do in Slug. Underscores are NOT collapsed: they are content here, and
// orders__clean is a filename somebody typed on purpose.
//
// Only dashes are trimmed from the ends, not underscores. A leading `_` is a
// real naming convention for staging tables, and stripping it would be this
// same round-trip bug in a smaller font.
//
// Case survives for that same round trip, and it is the last place the loop
// folded: a table named `Orders` came back as orders.sql, so the file git was
// already tracking as Orders.sql became a SECOND file for one table — visible
// only on a case-SENSITIVE filesystem, which is where CI runs. Collisions are
// still resolved case-INSENSITIVELY by uniqueSQLPath, because `Orders` and
// `orders` are one file on macOS and Windows.
func FileSlug(name, fallback string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return fallback
	}
	// The same cap Slug applies, and for the same reason: a 400-character table
	// name is a plausible thing for a human to write, and a filename is not the
	// place to find out what the filesystem's limit is.
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		return fallback
	}
	return slug
}
