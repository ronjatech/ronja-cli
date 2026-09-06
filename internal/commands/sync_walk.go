package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Why a folder in the tree was not checked.
//
// These strings are the `reason` field of the --json output, so they are a
// contract with whatever reads it — the same rule bind.go's reason constants
// follow. Every one of them is NEVER GREEN: a not_checked folder contributes
// `unknown` to the tree verdict, because the failure this vocabulary replaces is
// a `--stack` typo reporting a whole repository clean when not one folder
// recognised the name.
const (
	// syncReasonNestedRoot — a folder root inside another folder root. See
	// discoverFolders for why 4a refuses to guess between the two readings.
	syncReasonNestedRoot = "nested_root"
	// syncReasonUnreadable — the manifest would not load at all: an unknown
	// kind, unparseable JSON, or a formatVersion above what this build reads.
	// Reported rather than skipped, because a folder this command cannot open
	// is precisely a folder it cannot vouch for.
	syncReasonUnreadable = "unreadable"
	// syncReasonKindNotCovered — a kind this build fully understands, that
	// `ronja sync` has no leg for. Today that is `module` and nothing else.
	//
	// Distinct from syncReasonUnreadable on purpose. "This build does not
	// understand your folder" and "this command does not cover your folder"
	// send a reader to opposite places — the first to a CLI upgrade, the second
	// to `ronja module status`, which answers for that folder perfectly well.
	// Both are still NEVER GREEN: a module folder nobody checked is a module
	// folder nobody checked, and the tree verdict has to say so.
	syncReasonKindNotCovered = "kind_not_covered"
	// syncReasonStackElsewhere — the folder declares the requested stack, and it
	// points at a different instance or organization than this credential
	// reaches. A repository legitimately holds folders belonging to several
	// organizations, so this is per-folder and never a whole-run abort.
	syncReasonStackElsewhere = "stack_elsewhere"
	// syncReasonStackAbsent — the folder names stacks and none of them is the
	// one asked for. Without this the folder would fall through to an unbound
	// selection and render as a clean "not bound here".
	syncReasonStackAbsent = "stack_absent"
	// syncReasonStackUnverified — the folder declares the requested stack and
	// names an organization for it, and THIS credential's organization could not
	// be established, so the two cannot be compared. See resolveStackForFolder.
	syncReasonStackUnverified = "stack_unverified"
	// syncReasonUnnamedStack — the folder names no stacks at all: the legacy
	// instances[] shape, or one that has never been pushed anywhere. There is
	// nothing for a stack name to select.
	syncReasonUnnamedStack = "unnamed_stack"
)

// walkSkipDirs are pruned by NAME, everywhere in the tree, whatever kind of
// folder they turn out to sit in.
//
// ⚠️ Deliberately NOT wfdir.Kind.SkipDirs, which is the obvious-looking reuse
// and is unimplementable here: SkipDirs is a property of a Kind, and at walk
// time the kind is not yet known — knowing it means having already read the
// manifest this walk is looking for. So the union is applied unconditionally.
// The cost of being wrong is bounded and one-directional: a ronja.json under a
// directory named `build` is not discovered, which is a folder nobody should be
// keeping there.
//
// Dot-prefixed directories are pruned too, .git above all — enormous, and never
// a place a folder root lives. Enumerate makes the same two exclusions for the
// same reasons (see wfdir.StructuralExclusion).
var walkSkipDirs = []string{"node_modules", "dist", "build"}

// discoveredFolder is one ronja.json found beneath the walk root.
type discoveredFolder struct {
	// Root is absolute, so nothing downstream depends on the process working
	// directory — the whole reason the status paths were root-parameterised.
	Root string
	// Rel is Root relative to the directory the walk started from, for display.
	// "." for a folder that IS the walk root.
	Rel string
	// Kind is the manifest's own, from the kind-agnostic probe. Zero for a
	// folder whose Reason is set before the kind could be established.
	Kind wfdir.Kind
	// Manifest is the loaded file, carried rather than re-read: the walk has to
	// load it anyway to establish that it CAN be loaded, and a second read is a
	// second answer — a file rewritten between the two would make the walk's
	// "readable" verdict describe a manifest nothing later acts on.
	//
	// Nil whenever Reason is set.
	Manifest *wfdir.Manifest
	// Reason is a sync* constant when this folder must not be checked, and
	// empty when it must. Detail carries the sentence a human reads.
	Reason string
	Detail string
}

// notChecked reports whether this folder was ruled out before any comparison.
func (d discoveredFolder) notChecked() bool { return d.Reason != "" }

// discoverFolders walks DOWN from dir for every ronja.json beneath it.
//
// A downward walk rather than a declared repository file, because a read-only
// command needs no repo-level declaration to do its job and a committed file
// format is the most expensive thing to get wrong — once a customer commits
// one, every later change to it is a migration.
//
// The walk collects EVERY root and post-processes nesting afterwards, rather
// than pruning at the first hit. Both readings of a nested root are defensible
// and they disagree: wfdir.FindRoot walks UP and takes the nearest ancestor, so
// the INNER folder is what every existing command run inside it acts on — while
// the outer folder's Enumerate genuinely includes the inner folder's files, so
// the outer folder's push would carry them. Reporting both would double-report
// the same files under two different bindings. So the outer root is checked
// normally, the inner one is `nested_root` (never green), and the situation is
// named loudly rather than guessed at.
//
// Returns folders in path order, which is the order they are reported in.
func discoverFolders(dir string) ([]discoveredFolder, error) {
	base, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	info, err := os.Stat(base)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", base, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory — --dir takes the directory to walk", base)
	}

	var roots []string
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is REPORTED rather than swallowed:
			// a tree command that silently walked past an unreadable subtree
			// would report the rest of the repository clean and say nothing
			// about the part it never saw.
			// ⚠️ It carries the UNKNOWN exit code, not the default 1. A tree that
			// could not be walked is the definition of "could not tell" — the
			// contract's own third value — and a plain error made `run` score it
			// 1, which tells a CI job something DRIFTED and invites the push that
			// overwrites the subtree nobody read.
			return withExitCode(syncExitUnknown, fmt.Errorf("walk %s: %w", path, err))
		}
		if !d.IsDir() {
			return nil
		}
		if path != base && skipWalkDir(d.Name()) {
			return fs.SkipDir
		}
		if _, err := os.Stat(wfdir.ManifestPath(path)); err == nil {
			roots = append(roots, path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			// Same reasoning as the walk error above: a manifest that cannot even
			// be stat'ed is a folder nobody checked.
			return withExitCode(syncExitUnknown, fmt.Errorf("read %s: %w", wfdir.ManifestPath(path), err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sorted so a nested root always follows its parent, which is what makes the
	// report read top-down. ⚠️ enclosingRoot is still a pairwise O(n²) scan —
	// this ordering is for the READER, not an algorithmic shortcut, and an
	// earlier comment claimed otherwise. It is fine at the sizes a repository
	// reaches; if it ever is not, the sort is what a single backward scan would
	// be built on.
	sort.Strings(roots)
	out := make([]discoveredFolder, 0, len(roots))
	for _, root := range roots {
		rel, relErr := filepath.Rel(base, root)
		if relErr != nil {
			rel = root
		}
		folder := discoveredFolder{Root: root, Rel: filepath.ToSlash(rel)}
		if outer := enclosingRoot(roots, root); outer != "" {
			folder.Reason = syncReasonNestedRoot
			folder.Detail = fmt.Sprintf("this folder sits inside the one at %s — a command finds its folder by walking UP to the nearest %s, so anything run in here acts on THIS folder, while a push from the outer one would carry these same files. Move one of them, or point --dir at the one you mean",
				outer, wfdir.ManifestName)
			out = append(out, folder)
			continue
		}
		kind, knownKind, kindErr := folderKindAt(root)
		if kindErr != nil {
			folder.Reason = syncReasonUnreadable
			folder.Detail = kindErr.Error()
			out = append(out, folder)
			continue
		}
		if !knownKind {
			// Refused HERE rather than by LoadManifest, whose message is written
			// for a single-kind command ("this command only understands kind
			// workflow") and is simply false about a tree command that
			// understands every kind in the registry. The Kind is left ZERO so
			// the report does not print "workflow" about a folder that declared
			// something else — the probe's fallback is for `bind`, not for a
			// reader.
			//
			// The list of kinds is folderKindLabels() rather than prose, for the
			// reason that function records: written out, it named three kinds
			// while this walk covered four.
			folder.Reason = syncReasonUnreadable
			folder.Detail = fmt.Sprintf("%s does not declare a kind this build understands — `ronja sync` checks %s folders, and a `ronja db` folder keeps no committed manifest at all",
				wfdir.ManifestPath(root), folderKindLabels())
			out = append(out, folder)
			continue
		}
		folder.Kind = kind
		// A module folder is understood and not covered, and it must not reach
		// the dispatch below: both status and check end in a `default:` arm
		// (workflow and pipeline respectively), so an uncaught module folder
		// would not be skipped — it would be silently REPORTED ON as a workflow,
		// against workflow endpoints, using a module's id.
		//
		// Answering for one is real work rather than a missing case: `status`
		// would need a root-parameterised module status report and a verdict
		// fold, and `check` would need to state that a marker-free kind makes no
		// references at all. Until that exists, saying so is the honest answer.
		//
		// The Kind is KEPT rather than zeroed the way the unknown-kind branch
		// above zeroes it: there the fallback is WorkflowKind and printing it
		// would be a lie, whereas here the probe established the real kind and
		// both report lines stamp Kind.Name, so the reader gets `"kind":
		// "module"` beside the reason instead of an empty string.
		if kind.Name == wfdir.KindModule {
			folder.Reason = syncReasonKindNotCovered
			folder.Detail = fmt.Sprintf("%s is a module folder, and `ronja sync` covers workflow, data-app and pipeline folders — run `ronja module status` in it instead",
				wfdir.ManifestPath(root))
			out = append(out, folder)
			continue
		}
		// The kind probe reads one key and forgives everything else, so a
		// manifest that will not decode, declares a format version this build
		// cannot read, or carries the old-style "instances" object is still
		// waiting here. LoadManifest is the one that says so properly.
		manifest, loadErr := wfdir.LoadManifest(root, kind)
		if loadErr != nil {
			folder.Reason = syncReasonUnreadable
			folder.Detail = loadErr.Error()
			out = append(out, folder)
			continue
		}
		folder.Manifest = manifest
		out = append(out, folder)
	}
	return out, nil
}

// skipWalkDir reports whether a directory NAME is pruned outright.
func skipWalkDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, skip := range walkSkipDirs {
		if name == skip {
			return true
		}
	}
	return false
}

// enclosingRoot names the folder root that contains this one, or "" when it is
// top-level. roots must be sorted.
//
// Compared on the path separator, not on the raw prefix: `/repo/orders` is not
// inside `/repo/order`, and a bare strings.HasPrefix would say it was.
func enclosingRoot(roots []string, root string) string {
	for _, candidate := range roots {
		if candidate == root {
			continue
		}
		if strings.HasPrefix(root, candidate+string(filepath.Separator)) {
			return candidate
		}
	}
	return ""
}

// resolveStackForFolder decides whether one folder can answer for the stack the
// tree command was asked about, WITHOUT ever aborting the run.
//
// This is the mechanic a tree command lives or dies on. One --stack is typed;
// the folders each declare their own, and nothing makes them agree. Today
// wfdir's own selectNamed hard-errors on a URL or organization mismatch — right
// for a single folder, fatal for a repository that legitimately holds folders
// belonging to several organizations — and a folder that simply lacks the name
// falls through to an unbound Selection, which a naive tree renderer would show
// as "not bound here": clean, exit 0. A --stack typo would then report a whole
// repository healthy.
//
// So every answer here is either "check it" (empty reason) or a not_checked
// reason that is never green. It mirrors selectNamed's own acceptance rules
// rather than re-deciding them, so a folder this says yes to is one selectNamed
// will also accept.
//
// want == "" means no --stack was given, and every folder is checked: the
// ordinary (url, organization) match then applies per folder, and a folder that
// matches nothing here reports itself unbound, which is already non-green.
//
// ⚠️ That last clause is TRUE, and it is narrower than it reads — it was misread
// once, so it is worth spelling out. It rests on the `if !f.Bound` branch in each
// of pipelineRemoteStatus / remoteStatus / appRemoteStatus, which sets a
// NotCheckedReason that is NOT that loop's nothing-deployed-yet one, so
// verdictOfPipelineStatus and verdictOfFileDrift both fall through to `unknown`.
// It says nothing whatever about a folder that IS bound here and has simply
// never been pushed: that is the nothing-yet reason, it used to be read as clean
// unconditionally, and it is neverDeployedVerdict that answers it now.
//
// `ronja sync check` does not rely on this at all — it refuses an unbound folder
// by name (syncReasonNotBoundHere), because its own alias leg would otherwise
// give a definite verdict about an organization it cannot see.
func resolveStackForFolder(m *wfdir.Manifest, lock *wfdir.Lock, key wfdir.InstanceKey, want string) (reason, detail string) {
	if want == "" {
		return "", ""
	}
	if stack, declared := m.Stacks[want]; declared {
		// The INSTANCE is compared unconditionally; the ORGANIZATION has three
		// answers here rather than selectNamed's two.
		//
		// ⚠️ Through wfdir.Matches, NEVER a raw string compare, and with the
		// tenant halves deliberately blanked so only the URLs are read — the same
		// idiom selectNamed uses two files over, for the same reason. ronja.json
		// is HAND-EDITABLE, so `https://Ronja.example.com/` and
		// `https://ronja.example.com` are one instance to everybody except `!=`.
		// A raw compare made that folder `stack_elsewhere`, skipped it, and exited
		// 2 with a diagnostic printing two URLs a reader cannot tell apart —
		// which is the worst shape a refusal can take.
		declaredKey := stack.Key()
		switch {
		case !wfdir.Matches(wfdir.InstanceKey{URL: declaredKey.URL}, wfdir.InstanceKey{URL: key.URL}):
			return syncReasonStackElsewhere, fmt.Sprintf("stack %q here is on %s, and this credential reaches %s",
				want, declaredKey.URL, key.URL)
		case key.Known() && declaredKey.TenantID != key.TenantID:
			return syncReasonStackElsewhere, fmt.Sprintf("stack %q here belongs to organization %s, and this credential reaches %s",
				want, declaredKey.TenantID, key.TenantID)
		case !key.Known() && declaredKey.TenantID != "":
			// ⚠️ THE ORGANIZATION IS UNKNOWN AND THE STACK NAMES ONE, so nothing
			// here can say whether they are the same. selectNamed treats that as
			// "not a mismatch" and carries on, which is right for a SINGLE folder:
			// it is the signed-out case, the caller is standing in the folder they
			// meant, and the local half of the report is still worth having.
			//
			// It is wrong for a tree. `sync check` answers its dependency leg with
			// no network at all, so an unresolved organization does not stop it
			// giving a confident `broken` about a folder belonging to somebody
			// else — the same confidently-wrong answer as the no-flag path,
			// reached through a failed /me instead of a missing --stack. Both tree
			// commands already report `unknown` for a folder whose remote half
			// went unread, so this costs a REASON, not a verdict.
			return syncReasonStackUnverified, fmt.Sprintf("stack %q here belongs to organization %s, and this credential's organization could not be established — so nothing here can confirm the two are the same. Sign in (`ronja login`), or pass --profile to name one",
				want, declaredKey.TenantID)
		}
		return "", ""
	}
	// A lock entry under exactly this name with no stack declaring it: the ids
	// under it were created by a push whose lock write landed and whose manifest
	// write did not. selectNamed ADOPTS that rather than treating the folder as
	// unbound, and so does this — otherwise the one folder that most needs
	// looking at is the one skipped.
	if lock != nil {
		if _, recorded := lock.Stacks[want]; recorded {
			return "", ""
		}
	}
	if !m.UsesStacks() {
		return syncReasonUnnamedStack, fmt.Sprintf("this folder names no stacks, so there is no %q here to check — a first push, or `ronja bind --stack %s`, names one",
			want, want)
	}
	return syncReasonStackAbsent, fmt.Sprintf("this folder names stacks (%s) and none of them is %q",
		strings.Join(m.StackNames(), ", "), want)
}
