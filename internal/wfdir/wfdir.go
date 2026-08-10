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
	"fmt"
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
// than ignored: the two folder types are different resources with different
// semantics, and a command that pushed one's files into the other would be
// actively destructive.
const (
	KindWorkflow = "workflow"
	KindDataApp  = "dataapp"
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
	// DefaultEntrypoint matches the server's own default for a new resource of
	// this kind.
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
}

var (
	// WorkflowKind is a Python workflow folder.
	WorkflowKind = Kind{
		Name:              KindWorkflow,
		Label:             "workflow",
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
		DefaultEntrypoint: "App.tsx",
		SkipDirs:          []string{"node_modules", "dist", "build"},
	}
)

// DefaultEntrypoint matches the server's own default for a new workflow.
//
// Retained as a package-level constant because it is what `wf init` writes and
// what a manifest missing an entrypoint falls back to; new code should prefer
// Kind.DefaultEntrypoint.
const DefaultEntrypoint = "main.py"

// ManifestPath and StatePath locate the two files inside a folder root.
func ManifestPath(root string) string { return filepath.Join(root, ManifestName) }
func StatePath(root string) string    { return filepath.Join(root, StateDirName, StateFileName) }

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
