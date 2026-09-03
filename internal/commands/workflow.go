package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja wf` is the one place the CLI owns more than a bootstrap.
//
// The design rule is "sync verbs yes, resource verbs no" — discovery, reads and
// one-shot writes belong on plain HTTP, and wrapping them would make the CLI a
// permanently incomplete mirror of the API. What justifies these commands is
// the same thing that justifies `login`: a loop HTTP does badly on its own.
// Editing a workflow from a folder is stateful (which row am I bound to, on
// which instance), multi-step (checkout, per-file sync, commit) and needs a
// drift baseline that lives on disk. Discovery still is not: there is
// deliberately no `wf list` and no `wf delete`.
func newWorkflowCmd() *cobra.Command {
	wf := &cobra.Command{
		Use:     "workflow",
		Aliases: []string{"wf"},
		Short:   "Develop a Ronja workflow from a local folder",
		Long: `Develop a Ronja workflow from a local folder.

A workflow folder holds your Python source plus a small manifest (ronja.json)
recording the workflow's title, its entrypoint, and which workflow it maps to
on each instance. The sync baseline lives in .ronja/, which is local-only and
git-ignored for you.

Two ways in:

  ronja wf clone <workflow-id>     copy an existing workflow down to a folder
  ronja wf init --feature <id>     turn a script you already have into one

Then edit locally and check where you stand:

  ronja wf status                  local changes, remote state, and drift
  ronja wf validate                check the folder server-side, save nothing
  ronja wf push                    sync the folder into your draft
  ronja wf test                    run your draft and report what happened
  ronja wf publish                 take the draft live (or ask an admin to)
  ronja wf run                     run the live workflow and report what happened
  ronja wf discard                 throw your draft away

Bindings are per-instance, so the same folder can target a local backend and
production without either overwriting the other's binding.`,
	}
	wf.AddCommand(
		newWorkflowInitCmd(), newWorkflowCloneCmd(), newWorkflowStatusCmd(),
		newWorkflowValidateCmd(), newWorkflowPushCmd(), newWorkflowTestCmd(),
		newWorkflowPublishCmd(), newWorkflowRunCmd(), newWorkflowDiscardCmd(),
	)
	addStackFlag(wf)
	return wf
}

// resolveInstance is the standard preamble: work out which instance we are
// talking to and refuse early if there is no credential for it.
func resolveInstance() (*config.Resolved, error) {
	resolved, err := config.Resolve(flagURL, flagProfile)
	if err != nil {
		return nil, err
	}
	if resolved.Token == "" {
		return nil, notSignedIn(resolved)
	}
	return resolved, nil
}

// ensureTenant fills in the ORGANIZATION, which the wf commands need because a
// workflow folder's binding is keyed by it: guessing it wrong produces a
// manifest whose binding no command will ever match — or, worse, a push that
// creates a duplicate workflow in the wrong place.
//
// Under a $RONJA_TOKEN credential it is asked of the SERVER, and never taken
// from a profile. The two are independent: `eval "$(ronja env)"` exports a
// token, and a later `ronja profile use other-org` in the same shell would
// otherwise bind one organization's workflow id to the other's credential.
//
// Called at the point the key is first NEEDED rather than as a preamble. For the
// clone/init sites that is still before any binding exists, so their cheap local
// refusals — a destination directory that is not empty — happen before anything
// goes over the network. folder.resolveBinding is the one caller that has to ask
// EARLY, because the lookup it makes next is wrong without the answer; see the
// note there. A credential that already records its organization — most stored
// profiles — costs no request at all.
//
// `ronja api` and `ronja query` never call it: they key nothing locally, so the
// organization is the server's business.
func ensureTenant(ctx context.Context, resolved *config.Resolved) error {
	if resolved.TenantID != "" {
		return nil
	}
	me, err := api.New(resolved.URL, resolved.Token).Me(ctx)
	if err != nil {
		return fmt.Errorf("ask %s which organization this token belongs to: %w", resolved.URL, err)
	}
	if me.Tenant == nil {
		return fmt.Errorf("this token belongs to no organization, so there is nothing to work with — join one in the web app first")
	}
	resolved.TenantID = me.Tenant.ID
	return nil
}

// folder is an opened synced folder — a workflow's or a data app's — plus the
// instance it is being read against.
type folder struct {
	Root string
	// Kind is what this folder describes. It decides which manifest kind is
	// accepted, which files are syncable, and which binding field names the row,
	// so it is carried rather than re-derived: a helper that guessed would be one
	// place for the two kinds to disagree.
	Kind     wfdir.Kind
	Manifest *wfdir.Manifest
	// Lock is ronja.lock.json — the committed, machine-owned state for each
	// named stack. Empty for a legacy instances[] folder, which keeps its ids
	// inline and never grows the file.
	Lock     *wfdir.Lock
	State    *wfdir.State
	Resolved *config.Resolved
	// Stack is the NAME of the stack this command is acting on, empty for a
	// legacy unnamed instances[] entry. It is what --stack selected, or what the
	// (url, tenantID) match resolved to.
	Stack string
	// Key is what the manifest and baseline are looked up by: this instance and
	// this organization.
	//
	// The baseline in .ronja/state.json is still keyed by this pair and NOT by
	// the stack name, which holds because Manifest.selectNamed refuses to CREATE
	// a second stack on an (instance, organization) this folder already names —
	// so the two keys are 1:1 for every folder this CLI wrote, which is what let
	// the local baseline stay exactly as it was through this change.
	//
	// ⚠️ It is no longer 1:1 by CONSTRUCTION. A merge can bring two stacks onto
	// one pair, and that is deliberately not refused at load — a folder must
	// stay openable, and `--stack <name>` must stay a recovery (see
	// wfdir.checkStacks). Such a folder shares one baseline between two stacks:
	// each command still compares against the last sync made HERE, so the
	// symptom is spurious drift rather than a wrong row. Closing it means keying
	// the baseline by stack name; see BL-4e71.
	Key wfdir.InstanceKey
	// Binding and Bound describe this folder's link to Key. Unbound is an
	// ordinary state, not an error: it means the folder has never been pushed
	// there.
	Binding wfdir.Binding
	Bound   bool
	// Bind is the selected stack's alias → resource id map, straight off
	// Selection.Bind: CONFIG, read-only, never written back by a push.
	Bind map[string]string
	// Codec translates between the folder's committed ALIAS form and the ids one
	// organization's server speaks. Built here, from Bind, so every command holds
	// the same one and none of them builds a second — see alias.go for the
	// invariant it exists to keep.
	//
	// Empty (and therefore the identity) for every folder that declares no
	// dependencies, which is every folder in the field today.
	Codec aliasCodec
	// BindingErr is set only when the ORGANIZATION is unknown and the folder is
	// bound to several on this instance.
	//
	// It used to say here that only a caller which skipped resolveInstance could
	// reach it — the signed-out half of `wf status`. That was wrong, and wrong in
	// the expensive direction: resolveInstance picks an INSTANCE and checks a
	// credential exists, and never resolved an organization at all, so every
	// command reached this with an unknown one under an environment token. The
	// push paths then read "ambiguous" as "not bound here" and reported a missing
	// featureID — advice to add a key the file already had. openFolder now
	// refuses it by name; `status` reads it deliberately, to degrade.
	BindingErr error
	// OrgErr is set only by openFolderForStatus, and only when asking the server
	// which organization this credential belongs to FAILED — a revoked or expired
	// token, a 5xx, no network.
	//
	// It is a reason, not a refusal, because the command holding it is `status`:
	// the local half of that report needs no server at all, and the one command
	// you run when you are already suspicious must not be the one that stops
	// answering when your token turns out to be dead. The acting commands go
	// through openFolder and still refuse — there the answer is load-bearing.
	OrgErr error
}

// openFolder finds the folder root from the working directory, loads both
// files, pins the folder to ONE organization, and refuses when it cannot.
//
// The manifest is required; a missing baseline is not (a colleague who cloned
// the repo from git correctly has no .ronja/).
//
// This is what every command that ACTS on the bound row uses, so both steps
// happen at one site rather than at fourteen — and a command added later
// inherits them. The two shallower openers below exist for the callers that
// legitimately want less, and each says which.
func openFolder(ctx context.Context, resolved *config.Resolved, kind wfdir.Kind) (*folder, error) {
	f, err := openFolderLocally(resolved, kind)
	if err != nil {
		return nil, err
	}
	if err := f.resolveBinding(ctx); err != nil {
		return nil, err
	}
	if err := f.requireOneOrganization(); err != nil {
		return nil, err
	}
	if err := f.requireNamedStack(); err != nil {
		return nil, err
	}
	if err := f.adoptStack(); err != nil {
		return nil, err
	}
	return f, nil
}

// requireNamedStack refuses an acting command on a stack folder that matches
// nothing here and was given no name.
//
// A folder that has moved to stacks must not grow an unnamed instances[] entry
// beside them: both shapes would answer for one place and which one a later
// command read would depend on nothing legible. The name is the one thing that
// cannot be derived, so it is asked for — and asked for HERE, before the push
// path reaches its own "no featureID recorded" refusal, which would send the
// reader to add a key to an entry that does not exist.
func (f *folder) requireNamedStack() error {
	if f.Bound || flagStack != "" || !f.Manifest.UsesStacks() {
		return nil
	}
	return fmt.Errorf("%s names stacks (%s) and none of them is %s. Name the stack this should deploy to with --stack <name> — an existing one to point this credential at it, or a new one to add an environment",
		wfdir.ManifestName, strings.Join(f.Manifest.StackNames(), ", "), describeTarget(f.Resolved))
}

// adoptStack persists the migration a --stack on a legacy folder ASKS for.
//
// Naming is the whole of the migration: an instances[] entry has everything a
// stack needs except a name, and a name cannot be derived — deriving one is how
// an invented word ends up in a committed file and in a colleague's --stack
// argument. So the moment a person types --stack against an entry that has none,
// they have supplied the missing half, and the folder is written in the new
// shape.
//
// Here rather than at each write site because a push only saves the manifest
// when it CREATES something: a `--stack dev push` against a folder already bound
// would otherwise resolve through the stack, sync every file, and leave the file
// on disk in the old shape — the flag doing nothing visible, for ever.
//
// On openFolder and NOT on openFolderForStatus: `status` is the command you run
// when you are already suspicious, and it does not get to rewrite committed
// files. Acting commands do, and this is idempotent.
//
// Only for a BOUND entry. An unbound folder has no binding to move, and writing
// a stack with an empty featureID would put a half-answer in the manifest before
// the push that fills it in has been allowed to fail.
func (f *folder) adoptStack() error {
	if f.Stack == "" || !f.Bound {
		return nil
	}
	if _, declared := f.Manifest.Stacks[f.Stack]; declared {
		return nil
	}
	if !f.Key.Known() {
		// The entry being named carries no organization — a hand-written
		// instances[] line matched on URL alone. A stack has to name one
		// (LoadManifest refuses otherwise), so writing this would leave a folder
		// the next command cannot open. Silent rather than an error: the command
		// itself is working, and the migration is an offer, not the job.
		return nil
	}
	f.recordBinding(f.Binding)
	f.adoptLiveHashes()
	if err := f.saveFolder(); err != nil {
		return fmt.Errorf("record stack %q in %s: %w", f.Stack, wfdir.ManifestName, err)
	}
	// Said out loud, on stderr, because it changes the FORMAT of a file in the
	// customer's repo, and a colleague on an older CLI will be refused by it
	// rather than silently misreading it.
	//
	// The version named is what the manifest WAS ACTUALLY STAMPED WITH — read
	// back off the saved file rather than taken from ManifestFormatVersion,
	// which is the highest version this build can read and not the one this
	// folder now needs. A dependency-less folder adopting a stack is stamped 2;
	// naming the constant told its author their whole team had to be on a
	// version-3 CLI for a change that never required one, which is exactly the
	// upgrade nobody has time for and the message loses its credibility over.
	fmt.Fprintf(os.Stderr, "  %s now names %s as stack %q, and %s records what deploys to it. Both are committed; everyone working on this folder needs a ronja new enough to read format version %d.\n",
		wfdir.ManifestName, describeTarget(f.Resolved), f.Stack, wfdir.LockName, f.Manifest.StampedFormatVersion())
	return nil
}

// openFolderForStatus is openFolder without the refusals, for the three
// `status` commands. It TRIES to resolve the organization, and neither an
// ambiguous binding nor a failed lookup stops it: both are recorded and
// reported, and the local half of the report is delivered either way.
//
// That is the whole point of a command you run when you are already suspicious.
// A CI pre-flight running `status --json` under a rotated token used to print
// the local diff plus a reason; aborting instead would make the one command that
// could explain the situation the one that says nothing.
func openFolderForStatus(ctx context.Context, resolved *config.Resolved, kind wfdir.Kind) (*folder, error) {
	root, err := folderRootHere(kind)
	if err != nil {
		return nil, err
	}
	return openFolderForStatusAt(ctx, root, resolved, kind)
}

// openFolderForStatusAt is openFolderForStatus for a root the caller already
// knows, rather than the one the PROCESS happens to be standing in.
//
// It exists for `ronja sync`, which reports on every folder beneath a
// directory and therefore cannot use the working directory as the answer. The
// obvious alternative — chdir per folder — is not one: the working directory is
// process-global, so it cannot be combined with any concurrency and would
// corrupt a concurrent run's idea of where it is. Threading the root is the
// only version of this that stays true when a second goroutine exists.
func openFolderForStatusAt(ctx context.Context, root string, resolved *config.Resolved, kind wfdir.Kind) (*folder, error) {
	f, err := openFolderLocallyAt(root, resolved, kind)
	if err != nil {
		return nil, err
	}
	// Swallowed, not propagated: resolveBinding has already matched the binding
	// with whatever organization it managed to establish, which for an unknown
	// one is the same URL-only match a signed-out caller gets.
	f.OrgErr = f.resolveBinding(ctx)
	// A bad --stack is the one thing status does NOT degrade around. Degrading is
	// for an environment that will not answer — a dead token, no network, an
	// organization nobody can establish — where the local half of the report is
	// still worth having. A stack that does not exist, or points somewhere this
	// credential cannot reach, is a wrong ARGUMENT: there is no report to give
	// about it, and printing one under the "bound to several organizations"
	// heading would describe a problem the reader does not have.
	if f.BindingErr != nil && !errors.Is(f.BindingErr, wfdir.ErrAmbiguousInstance) {
		return nil, f.BindingErr
	}
	return f, nil
}

// openFolderLocally loads the folder and NOTHING ELSE: no request, and no
// promise about Binding, Bound or Key, which are matched against whatever
// organization the credential happened to arrive knowing.
//
// One caller, `app fork`, and it qualifies for a specific reason rather than by
// being cheap: it copies a kit file into the folder and never reads the binding
// at all, so an organization it cannot name cannot make it do the wrong thing.
// Anything that touches Binding must go through resolveBinding first.
func openFolderLocally(resolved *config.Resolved, kind wfdir.Kind) (*folder, error) {
	root, err := folderRootHere(kind)
	if err != nil {
		return nil, err
	}
	return openFolderLocallyAt(root, resolved, kind)
}

// folderRootHere is the working-directory half of the two openers: walk UP for
// the nearest ronja.json, and say something useful when there is none.
//
// Split out so the cwd coupling lives at exactly one site. Everything below it
// takes a root, which is what let `ronja sync` open forty folders in one
// process without a chdir — see openFolderForStatusAt.
func folderRootHere(kind wfdir.Kind) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("locate working directory: %w", err)
	}
	root, err := wfdir.FindRoot(cwd)
	if err != nil {
		if errors.Is(err, wfdir.ErrNoManifest) {
			return "", fmt.Errorf("no %s here or in any parent directory — run this inside a folder created by `%s clone` or `%s init`",
				wfdir.ManifestName, kind.Command, kind.Command)
		}
		return "", err
	}
	return root, nil
}

// openFolderLocallyAt is openFolderLocally for a root the caller already knows.
// See openFolderForStatusAt for why the root is threaded rather than chdir'd to.
func openFolderLocallyAt(root string, resolved *config.Resolved, kind wfdir.Kind) (*folder, error) {
	// LoadManifest refuses a folder of the OTHER kind, which is the guard that
	// keeps `ronja wf push` from creating a workflow out of a data app's TSX.
	manifest, err := wfdir.LoadManifest(root, kind)
	if err != nil {
		return nil, err
	}
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		return nil, err
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		return nil, err
	}
	f := &folder{Root: root, Kind: kind, Manifest: manifest, Lock: lock, State: state, Resolved: resolved}
	f.matchBinding()
	return f, nil
}

// resolveBinding pins the folder to one organization before its binding is
// read, which is the difference between a lookup that is right and one that is
// merely plausible.
//
// It asks whenever the credential does not already name its organization, which
// an environment token never does by construction (config.Resolve clears it —
// the token and the profile are independent, so a profile's organization says
// nothing about which one $RONJA_TOKEN reaches) and a stored profile can equally
// fail to: login writes an empty tenantID for a user who belonged to no
// organization yet, and deliberately never rewrites it afterwards. Gating on the
// environment token alone left that profile going through the same door. Left
// unknown, matchBinding matches on URL ALONE, adopts whichever organization's
// binding this folder happens to name, and the push then sends that
// organization's resource ids under this token. That is the exact failure
// InstanceKey exists to prevent, and it was defeated on the one credential path
// CI is documented to use.
//
// A credential with NO token is not asked at all: it has nothing to ask with,
// and the signed-out half of `status` is a supported state whose answer is the
// URL-only match — not a 401 dressed up as an organization problem.
//
// Asked only when there is something to adopt — an entry on this instance. A
// folder that names none cannot adopt a wrong one, and pays nothing.
//
// The cost the old lazy version defended is REAL and is partly given up,
// deliberately: under an environment token a folder that IS bound here now
// spends one GET /me before a purely local refusal (a file with null bytes, one
// too big to push). `wf init` records the feature binding, so that is most
// folders. The property cannot survive alongside a correct lookup — the binding
// is read immediately after this, and reading it wrong is how another
// organization's ids reach this token — and the alternative, an opt-in call at
// every site that reads a binding, is the exact shape that let the bug exist. A
// STORED PROFILE still asks nothing at all, which is the half of the promise
// that mattered: the paid request belongs only to the credential that genuinely
// does not know its own organization.
// The binding is matched WHETHER OR NOT the lookup succeeded, and the error is
// returned rather than short-circuiting, so a caller that chooses to degrade
// (`status`) still has a folder to report on. openFolder refuses on it.
//
// ⚠️ A DECLARED --stack does NOT skip it, and the temptation to let it is worth
// naming. A stack states its own organization, so the round trip looks
// redundant — but the two answer different questions. The stack says which
// organization this folder MEANS; /me says which one this token REACHES, and a
// stack whose declaration is stale, or a token exported for the wrong
// organization, is exactly the case where those differ. Asked, the disagreement
// becomes Manifest.Select's refusal; skipped, the push resolves the stack's
// featureID under a token that cannot see it, and every push path reads the
// resulting 404 as "create it". One request is the right price for that, and it
// is the price the credential without --stack already pays.
//
// What --stack buys in CI is therefore not a saved request: it is that a
// mismatch is REFUSED by name instead of silently adopting whatever binding the
// committed file happens to carry.
func (f *folder) resolveBinding(ctx context.Context) error {
	var err error
	if f.Resolved.Token != "" && f.Resolved.TenantID == "" && f.hasEntryOnThisInstance() {
		err = ensureTenant(ctx, f.Resolved)
	}
	f.matchBinding()
	return err
}

// hasEntryOnThisInstance reports whether the folder names ANYTHING on this
// instance — a stack or a legacy entry. It is the "is a round trip worth it"
// gate: a folder that names nothing here cannot adopt a wrong binding, and pays
// nothing.
func (f *folder) hasEntryOnThisInstance() bool {
	if len(f.keysOnThisInstance()) > 0 {
		return true
	}
	// A stack the LOCK records and the manifest does not name is a real binding
	// too — the ids a push created before its manifest write was lost, which
	// selectNamed adopts rather than duplicating. The lock carries no url of its
	// own, so "on this instance" cannot be asked of it; what stands in for it is
	// that a person typed the name, and the cost of being wrong is one GET /me.
	//
	// It matters because of what the answer UNLOCKS: without a resolved
	// organization the key stays unknown, adoptStack declines to write, and the
	// folder never heals — so its next push with no --stack sees a manifest
	// naming nothing, and creates the duplicate the adoption just avoided.
	if flagStack == "" {
		return false
	}
	_, recorded := f.Lock.Stacks[flagStack]
	return recorded
}

// orgNotCheckedReason is why a `status` command did not look at the server: the
// organization could not be established, so which of this folder's bindings
// applies is not known, and reading one anyway would report another
// organization's row as this folder's.
//
// One sentence for all three kinds — the failure is the same one and the three
// reports would otherwise drift apart. Empty when the organization is fine.
func (f *folder) orgNotCheckedReason() string {
	if f.OrgErr == nil {
		return ""
	}
	return fmt.Sprintf("%v — so which of this folder's bindings applies is unknown; the local half of this report is unaffected", f.OrgErr)
}

// matchBinding looks this folder's binding up for the organization currently
// known, and is re-run whenever that answer changes.
func (f *folder) matchBinding() {
	key := wfdir.InstanceKey{URL: f.Resolved.URL, TenantID: f.Resolved.TenantID}
	sel, err := f.Manifest.Select(f.Lock, key, flagStack)
	// The matched entry's organization is ADOPTED. When ours was already known
	// this changes nothing (the match was exact on it); when it was not, this is
	// the answer, and it cost no request. A declared --stack is the same move
	// made from the committed file rather than from a lookup.
	f.Stack, f.Key, f.Binding, f.Bound, f.BindingErr = sel.Name, sel.Key, sel.Binding, sel.Bound, err
	f.Bind = sel.Bind
	if err != nil {
		f.Stack, f.Key, f.Binding, f.Bound, f.Bind = "", key, wfdir.Binding{}, false, nil
	}
	// Rebuilt whenever the match is, because it is a function OF the match: a
	// folder that resolved to a different stack answers to a different set of
	// ids, and a codec left over from the previous answer would resolve this
	// organization's names to the last one's rows.
	f.Codec = newAliasCodec(f.Manifest.Dependencies, f.Bind)
}

// selection is this folder's resolved stack, in the shape wfdir writes back
// through. Built rather than stored so it always carries the CURRENT binding —
// the pipeline loop mutates f.Binding file by file and records after each one.
func (f *folder) selection() wfdir.Selection {
	// Bind travels with it so Manifest.CheckBind can be asked about the folder as
	// it stands. Manifest.Record ignores the field — a bind map is config no
	// deploy ever writes, which is exactly why it lives on Stack and not on
	// Binding (see Stack.Bind).
	return wfdir.Selection{Name: f.Stack, Key: f.Key, Binding: f.Binding, Bound: f.Bound, Bind: f.Bind}
}

// recordBinding writes a binding back into whichever pair of files this folder
// uses, and is the ONLY way a command should do it. Going through
// Manifest.Record is what keeps the config/state split from being a convention
// somebody forgets: featureID to ronja.json, created ids to ronja.lock.json, and
// both to instances[] for a legacy folder that has never named a stack.
func (f *folder) recordBinding(b wfdir.Binding) {
	f.Binding = b
	f.Manifest.Record(f.Lock, f.selection(), b)
}

// saveFolder writes both committed files. See wfdir.SaveFolder for why they are
// one write and why the lock goes first.
func (f *folder) saveFolder() error { return wfdir.SaveFolder(f.Root, f.Manifest, f.Lock) }

// bindingFiles names the committed file(s) a newly created resource id is
// recorded in, for the refusal a failed save has to print.
//
// Two shapes, two answers, and naming the wrong one sends a reader to a file
// that is very likely fine. A LEGACY folder keeps the id in ronja.json. A STACK
// folder keeps it in ronja.lock.json — ronja.json only gains the stack's
// featureID — and SaveFolder writes the lock FIRST, so a save that failed may
// well have failed there and left the manifest untouched.
func (f *folder) bindingFiles() string {
	if f.Stack == "" {
		return wfdir.ManifestPath(f.Root)
	}
	// Lock first, the order they are written in, so the likelier culprit reads
	// first.
	return wfdir.LockPath(f.Root) + " and " + wfdir.ManifestPath(f.Root)
}

// declaredStack is the stack name the folder's own COMMITTED files carry, empty
// when the resolved name is one this run supplied and nothing has written down.
//
// It exists for `status`, which deliberately does not migrate: a legacy folder
// given `--stack dev` resolves through the migration path and comes out with
// Stack == "dev", but ronja.json still declares no stacks and the command that
// would write one is not this one. Reporting `Stack: dev` there states a fact
// about the folder that is not true until an acting command makes it true.
//
// The LOCK counts as declaring it, because it is committed too: a stack whose
// manifest write was lost is really recorded, adopted by name on the next push,
// and reporting the name is the one thing that would explain the folder.
func (f *folder) declaredStack() string {
	if f.Stack == "" {
		return ""
	}
	if _, ok := f.Manifest.Stacks[f.Stack]; ok {
		return f.Stack
	}
	if _, ok := f.Lock.Stacks[f.Stack]; ok {
		return f.Stack
	}
	return ""
}

// adoptLiveHashes carries a pipeline folder's LIVE fingerprints across at the
// moment it is named, from the local baseline into the lock that now owns them.
//
// Without it a migrated folder would keep its fingerprints in .ronja/ — correct
// for whoever ran the migration, invisible to everybody else — until a fresh
// draft fork or a publish happened to re-record them. The whole claim of the
// lock file is that naming a stack gives CI something committed to compare
// against, and "eventually, after somebody publishes" is not that.
//
// One direction only, and only into an EMPTY slot: the lock is authoritative
// once it has a value, and a local baseline is exactly the thing that may be
// stale relative to it. Harmless for the two kinds that have no table state —
// their baselines carry no Tables map at all.
func (f *folder) adoptLiveHashes() {
	inst := f.State.For(f.Key)
	if inst == nil || f.Stack == "" {
		return
	}
	for path, tableID := range f.Binding.Tables {
		recorded := inst.TableStateFor(path)
		if recorded.LiveSHA256 == "" || recorded.TableID != tableID {
			// No fingerprint, or one taken from a DIFFERENT row: a hash is only
			// ever compared against the row it was taken from, so a mismatched
			// TableID means this value describes something else entirely.
			continue
		}
		if f.Lock.TableLive(f.Stack, path) != "" {
			continue
		}
		f.Lock.SetTableLive(f.Stack, path, tableID, recorded.LiveSHA256)
	}
}

// saveBaseline writes what a per-file loop just learned: the local baseline,
// and — for a named stack — the committed lock, which is where that folder's
// LIVE fingerprints now live.
//
// The lock half is easy to forget and silent when forgotten, which is why it is
// a helper rather than a second line at each site. `pipeline push` records a
// live agreement on the ordinary UPDATE path, not only on a create; before this
// existed the lock was written only when a table was created, so a stack
// folder's fingerprints were kept in memory, discarded at exit, and every
// subsequent push read "no baseline" and adopted whatever the live table held —
// the drift guard disarmed by its own writer.
//
// The MANIFEST is deliberately not written here. Nothing in a per-file loop
// changes config, and rewriting the file a reviewer reads on every push is the
// churn this whole split exists to stop.
//
// Lock first, for the reason wfdir.SaveFolder gives: a live fingerprint that
// landed without its baseline is a true statement about the environment, while
// a baseline claiming a sync whose fingerprint never landed reads as "no
// baseline" next time — recoverable, but only by re-adopting.
func (f *folder) saveBaseline() error {
	if err := wfdir.SaveLock(f.Root, f.Lock); err != nil {
		return err
	}
	return wfdir.SaveState(f.Root, f.State)
}

// recordFirstBinding writes a brand-new folder's binding into the shape --stack
// asks for, and hands back the lock file to save beside the manifest.
//
// The default is the LEGACY shape, and that is a deliberate choice about
// rollout rather than an oversight. A stack folder is manifest format version 2,
// which every CLI older than the version gate reads by silently dropping the key
// it does not know — leaving a manifest that names no binding at all, whose next
// push creates a second set of resources. Until the gate is common in the field,
// a folder becomes a stack folder because a person typed --stack, never because
// a `clone` felt modern.
//
// The one-line summary for whoever flips it later: change the condition here,
// not the format.
func recordFirstBinding(m *wfdir.Manifest, key wfdir.InstanceKey, b wfdir.Binding) (*wfdir.Lock, error) {
	lock := &wfdir.Lock{}
	if err := recordFirstBindingInto(m, lock, key, b); err != nil {
		return nil, err
	}
	return lock, nil
}

// recordFirstBindingInto is the same write against a lock the caller already
// holds — `pipeline clone`, which records live fingerprints into it as it walks
// the tables and so cannot be handed a fresh one afterwards.
func recordFirstBindingInto(m *wfdir.Manifest, lock *wfdir.Lock, key wfdir.InstanceKey, b wfdir.Binding) error {
	if flagStack != "" {
		if err := wfdir.ValidStackName(flagStack); err != nil {
			return err
		}
		// A stack is one organization on one instance, and LoadManifest refuses
		// one that names no organization — so writing an organization-less stack
		// here would produce a folder the very next command cannot open. Every
		// caller resolves the organization before it gets here, which makes this
		// a guard against a future one that forgets, not a case anybody hits.
		if !key.Known() {
			return fmt.Errorf("cannot name stack %q for %s: this credential's organization is not known, and a stack has to name one. Sign in with `ronja login`, or use a profile that records its organization",
				flagStack, key.URL)
		}
	}
	m.Record(lock, wfdir.Selection{Name: flagStack, Key: key, Binding: b}, b)
	return nil
}

// requireOneOrganization refuses a folder that names several organizations on
// this instance when nothing says which one applies.
//
// It exists because the alternative is silent and misleading. An ambiguous
// lookup answers "no binding", the push paths read that as "create it", and the
// message they produce is about a MISSING featureID — which sends the reader to
// add a key their ronja.json already carries, for the organization they are not
// pushing to.
//
// ONE selector, --profile: a stored profile records the organization, which is
// exactly the half the lookup is missing. There is deliberately no --instance or
// --target flag — two ways to name an organization is the same ambiguity one
// layer up, and it is the defect this refusal is about.
//
// It is a BACKSTOP on the acting paths rather than their usual answer: since
// resolveBinding asks for any credential that does not name its organization,
// an acting command now either knows one by the time it gets here or has already
// been refused by the lookup itself. It stays because "the organization is
// unknown" is the condition that must never reach a binding read, and a future
// caller that acquires an unknown one some other way must hit a refusal, not a
// coin flip.
func (f *folder) requireOneOrganization() error {
	if f.BindingErr == nil {
		return nil
	}
	if !errors.Is(f.BindingErr, wfdir.ErrAmbiguousInstance) {
		// Everything Select can refuse EXCEPT the organization ambiguity below is
		// already a complete sentence naming what it found and how to fix it — a
		// --stack that points elsewhere, a near-miss name, an ambiguity between
		// two named stacks. Wrapping any of them in the message below would say
		// the opposite of what happened: it opens "this credential does not say
		// which organization applies", which is false in every one of those
		// cases, and then lists the organizations with nothing to list.
		return f.BindingErr
	}
	keys := f.keysOnThisInstance()
	named := make([]string, 0, len(keys))
	for _, key := range keys {
		if key.TenantID == "" {
			// An entry with no tenantID is hand-written and could belong to
			// anybody, which is why Find will not adopt it either. Named as what
			// it is rather than as an empty string in a list.
			named = append(named, `an entry with no "tenantID"`)
			continue
		}
		named = append(named, key.TenantID)
	}
	// Wrapped, so errors.Is still recognises the ambiguity a caller may one day
	// want to branch on, and so the sentence the sentinel already carries is not
	// written out a second time here.
	pick := fmt.Sprintf("  Name one with --profile <name>: a stored profile records its organization, which is the half this lookup is missing. `ronja profile list` shows what you have, `ronja login --url %s` adds one", f.Resolved.URL)
	if stacks := f.Manifest.StackNames(); len(stacks) > 0 {
		// A stack name is offered FIRST when the folder has them, because it
		// lives in the repo: a CI job under $RONJA_TOKEN can name one with no
		// stored profile at all, and that is exactly the credential which never
		// records an organization. --profile stays on the line because it is
		// still the answer for a folder whose stacks nobody has named yet.
		pick = fmt.Sprintf("  Name one with --stack <name> (%s), or with --profile <name> — a stored profile records its organization, which is the half this lookup is missing",
			strings.Join(stacks, ", "))
	}
	return fmt.Errorf("%w on %s — %s names %d of them (%s), and this credential does not say which applies.\n%s",
		f.BindingErr, f.Resolved.URL, wfdir.ManifestPath(f.Root), len(named),
		strings.Join(named, ", "), pick)
}

// keysOnThisInstance lists every (instance, organization) this folder names on
// the resolved instance, stacks and legacy entries alike.
func (f *folder) keysOnThisInstance() []wfdir.InstanceKey {
	return f.Manifest.KeysOn(f.Resolved.URL)
}

// otherOrganizationsOn lists the organizations this folder names on this
// instance that are NOT the one we resolved to — the entries a reader will see
// when told their folder names none.
//
// Empty when the resolved organization is unknown, because "other than what"
// has no answer then and a list would read as an accusation.
func (f *folder) otherOrganizationsOn() []string {
	if f.Key.TenantID == "" {
		return nil
	}
	var out []string
	for _, key := range f.keysOnThisInstance() {
		if key.TenantID != f.Key.TenantID {
			out = append(out, key.TenantID)
		}
	}
	return out
}

// ResourceID is the row this folder is bound to on its instance, empty when it
// has never been pushed there.
//
// It errors for a folder kind that binds MANY rows (a pipeline), which is why
// the signature is not a bare string: that caller has to read Binding.Tables,
// and answering it with an empty id would read as "never pushed here".
//
// The command a kind's messages should name is wfdir.Kind.Command — one
// mapping, declared on the Kind itself. It used to be spelled here as well, as
// a binary if/else that would have routed a third kind to `ronja wf`.
func (f *folder) ResourceID() (string, error) { return f.Binding.ResourceID(f.Kind) }

// bindNew resolves the organization and returns the key a NEW binding should be
// written under. Only the paths that create a binding need this, and they are
// the only ones that must not guess: an unbound folder has nothing to adopt an
// organization from, and writing one with the wrong organization — or none —
// produces a manifest no later command matches.
//
// ⚠️ Call it only for an UNBOUND folder — `creating && !f.Bound`, which is the
// condition all three push loops apply. "Creating" is not the same question:
// a folder that declares a stack but has no row yet (`wf init --stack dev`, or
// a `discard --delete-workflow` that emptied the lock entry) is BOUND and
// creating, and it already knows both its organization and its stack. Calling
// this there re-derives both from the credential, which is where the split
// folder came from: the resolved stack was overwritten with an empty flagStack,
// the created id went to a legacy instances[] entry beside the declared stack,
// and every later command refused as ambiguous — the documented recovery
// (`--stack dev`) then creating a SECOND workflow.
func (f *folder) bindNew(ctx context.Context) (wfdir.InstanceKey, error) {
	if err := ensureTenant(ctx, f.Resolved); err != nil {
		return wfdir.InstanceKey{}, err
	}
	f.Key = wfdir.InstanceKey{URL: f.Resolved.URL, TenantID: f.Resolved.TenantID}
	if f.Stack == "" {
		// The stack name a person supplied, carried onto the new binding.
		// requireNamedStack has already refused the case where one was needed
		// and none was given, so an empty name here means a legacy folder —
		// which is the shape the new binding correctly takes.
		//
		// Never over an ALREADY-RESOLVED stack, as belt to the callers' braces:
		// a name Manifest.Select established is the answer, and flagStack is
		// either the same word or empty.
		f.Stack = flagStack
	}
	return f.Key, nil
}

// featureAdvice names WHERE a missing "featureID" goes in THIS folder's file,
// which is not the same sentence for the two shapes.
//
// "Add \"featureID\" to the <url> entry" describes a line a stack folder does not
// have — its features live at stacks.<name>.featureID — and sending a reader to
// edit a line that is not there is the exact defect the surrounding refusals
// were rewritten to stop. It arrives through a new door once a folder migrates,
// so the sentence is built from the shape rather than written out per site.
func (f *folder) featureAdvice() string {
	switch {
	case f.Stack != "":
		return fmt.Sprintf("Add \"featureID\" to the %q stack in %s", f.Stack, wfdir.ManifestPath(f.Root))
	case f.Manifest.UsesStacks():
		return fmt.Sprintf("Add a stack for %s to %s, with its own \"featureID\"", describeTarget(f.Resolved), wfdir.ManifestPath(f.Root))
	default:
		return fmt.Sprintf("Add \"featureID\" to the %s entry in %s", f.Resolved.URL, wfdir.ManifestPath(f.Root))
	}
}

// featureAdviceForAnotherOrganization is featureAdvice for the case where this
// folder names OTHER organizations here and none for this one — where the fix is
// a whole new entry rather than one more key on an existing line.
func (f *folder) featureAdviceForAnotherOrganization() string {
	if f.Manifest.UsesStacks() {
		return fmt.Sprintf("Name this one with --stack <name> on the next push, or add a stack for it to %s with its own \"featureID\"", wfdir.ManifestPath(f.Root))
	}
	return fmt.Sprintf("Add an entry with \"tenantID\": %q and its own \"featureID\"", f.Key.TenantID)
}

// describeTarget names where a command just wrote, as instance plus
// organization.
//
// Reported by the commands that CHANGE something, not only by the ones you run
// when you are already suspicious. `context` and `status` are what you check
// after the fact; a push is where sending a folder to the wrong organization
// actually happens, and it is worth one line to make that visible at the moment
// it occurs.
//
// The organization's NAME when a profile recorded one, its id otherwise (an
// environment token was never introduced to us).
func describeTarget(resolved *config.Resolved) string {
	org := ""
	if resolved.Entry != nil && !resolved.FromEnv {
		org = resolved.Entry.TenantName
	}
	if org == "" {
		org = resolved.TenantID
	}
	if org == "" {
		return resolved.URL
	}
	return fmt.Sprintf("%s on %s", org, resolved.URL)
}

// refuseUnclonable reports why a workflow row cannot back a folder, or nil when
// it can. Only live and draft rows are workable: everything else is either
// immutable or governed by a flow the CLI has no business half-implementing.
func refuseUnclonable(wf *api.Workflow) error {
	switch wf.Lifecycle {
	case api.LifecycleLive, api.LifecycleDraft:
		return nil
	case api.LifecycleProposed:
		return fmt.Errorf("workflow %s is a proposal awaiting admin approval — it is managed via the proposal flow, not from a folder",
			wf.ID)
	case api.LifecycleVersion:
		return fmt.Errorf("workflow %s is a committed version snapshot, not an editable workflow — restore it from the web UI, then clone the result",
			wf.ID)
	case api.LifecycleArchived:
		return fmt.Errorf("workflow %s is archived — restore it from the web UI first", wf.ID)
	default:
		return fmt.Errorf("workflow %s has lifecycle %s, which this command does not work with",
			wf.ID, api.DescribeLifecycle(wf.Lifecycle))
	}
}

// hashFiles fingerprints a fetched file set for baseline and drift comparison.
//
// De-aliased FIRST, because what these hashes are compared against is the hash
// of the bytes on disk — see alias.go. A remote hash taken before the id → alias
// step describes content the folder has never held, and every aliased file would
// read as drifted on every status, for ever.
func hashFiles(codec aliasCodec, files []api.WorkflowFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = wfdir.HashString(codec.toDisk(f.Content))
	}
	return out
}

// readLocalFiles enumerates a folder and reads the content of every syncable
// file, so the same set can be validated and pushed without walking twice.
//
// Content is held in memory on purpose: a workflow is source code, the server
// stores it in a text column, and the push guard below caps what may be sent
// anyway — so the whole folder is small by construction.
func readLocalFiles(root string, kind wfdir.Kind) (map[string]string, *wfdir.Enumeration, error) {
	enumeration, err := wfdir.Enumerate(root, kind)
	if err != nil {
		return nil, nil, err
	}
	files := make(map[string]string, len(enumeration.Files))
	for path := range enumeration.Files {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		files[path] = string(body)
	}
	return files, enumeration, nil
}

// maxFileBytes caps one WORKFLOW file, on both the push and validate paths.
//
// The file-SAVE path has no explicit limit — content is a text column — which
// is why the CLI needs one: pushing a 200 MB CSV somebody dropped in the folder
// fails somewhere deep in the request path with a message about nothing, and
// the diagnosis ("that is not source code") is entirely local. A megabyte is
// far beyond any hand-written Python file.
//
// The VALIDATE endpoint does have one, and it is deliberately this same number
// (rworkflow.maxValidateFileBytes) — so a folder this refuses is one the server
// would refuse anyway, less helpfully, and a folder it accepts is never
// rejected server-side for its size. Change both.
//
// Data apps have their own, LARGER limit (see appMaxFileBytes), which is why
// checkPushable takes the cap as an argument rather than reading this: refusing
// a 2 MB .tsx that rdataapp would have accepted is the same class of unhelpful
// local refusal, just pointed the other way.
//
// `ronja pipeline push` reuses this number for a .sql file, deliberately and on
// the first rationale rather than the second: a table's `code` is a text column
// with no server-side cap either, and a megabyte is far beyond any hand-written
// query. There is no pipeline VALIDATE endpoint to keep in step, so the "change
// both" rule above is about workflows only.
const maxFileBytes = 1 << 20

// checkPushable refuses a local file set that cannot be synced safely, BEFORE
// any request is made.
//
// `wf validate` runs it too, so the messages below say "synced" rather than
// "pushed": a validate that refused a 200 MB CSV by telling you to fix it and
// push again would be answering a question nobody asked.
//
// Three refusals, all hard rather than skip-with-a-warning, because in each
// case skipping silently produces a workflow that differs from the folder the
// author is looking at:
//
//   - Paths differing only by case. They are two rows server-side and ONE file
//     on macOS or Windows, so a colleague cloning this workflow there loses
//     one of them. Possible to create on Linux, so it is checked everywhere
//     rather than left to the filesystem to notice.
//   - Files over maxFileBytes, or containing NUL bytes. `content` is a text
//     column; binary content is either rejected with an opaque encoding error
//     or stored as something nobody can read back.
//
// The KIND is a parameter for the message alone, and only because the message
// was wrong: this runs for all three folder loops, and a data app refused for
// holding a PNG was told "a workflow holds source code" — naming a primitive the
// author is not working on, which reads as a bug in the CLI rather than as
// something they can act on. kind.Label is the same word every other message in
// this package uses for the folder, so nothing here decides on its own wording.
func checkPushable(files map[string]string, kind wfdir.Kind, maxBytes int) error {
	byFold := map[string][]string{}
	for path := range files {
		fold := strings.ToLower(path)
		byFold[fold] = append(byFold[fold], path)
	}
	var collisions []string
	for _, group := range byFold {
		if len(group) < 2 {
			continue
		}
		sort.Strings(group)
		collisions = append(collisions, strings.Join(group, " and "))
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		return fmt.Errorf("this folder has file paths that differ only by capitalisation, which are one file on a case-insensitive filesystem:\n    %s\n  Rename one of each pair before syncing",
			strings.Join(collisions, "\n    "))
	}

	var rejected, rejectedPaths []string
	for _, path := range sortedPaths(files) {
		content := files[path]
		switch {
		case len(content) > maxBytes:
			rejected = append(rejected, fmt.Sprintf("%s (%d bytes, limit %d)", path, len(content), maxBytes))
		case strings.ContainsRune(content, 0):
			rejected = append(rejected, path+" (binary content: contains null bytes)")
		default:
			continue
		}
		rejectedPaths = append(rejectedPaths, path)
	}
	if len(rejected) > 0 {
		return fmt.Errorf("these files cannot be synced to Ronja — a %s holds source code, not data or binaries:\n    %s\n%s  Remove them from the folder (or keep them out of it) and try again",
			kind.Label, strings.Join(rejected, "\n    "), appTestArtifactHint(kind, rejectedPaths))
	}
	return nil
}

// sortedPaths orders a file map's keys, so every listing and every request
// sequence is deterministic.
func sortedPaths(files map[string]string) []string {
	out := make([]string, 0, len(files))
	for path := range files {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// validateFilesOf turns a local file set into the validate request's file list.
//
// Resolved on the way out, like every other send: validate exists to report what
// a PUSH of these bytes would bind, and a candidate carrying unresolved aliases
// would be checked against names the push is never going to send.
func validateFilesOf(codec aliasCodec, files map[string]string) []api.ValidateFile {
	out := make([]api.ValidateFile, 0, len(files))
	for _, path := range sortedPaths(files) {
		out = append(out, api.ValidateFile{Path: path, Content: codec.toWire(files[path])})
	}
	return out
}

// bindingsOf reads a workflow row's stamped bindings into the same shape
// validate reports, so "what this would bind to" and "what it now binds to"
// print identically.
func bindingsOf(wf *api.Workflow) api.ValidateBindings {
	return api.ValidateBindings{
		InputTableIDs:  wf.InputTableIDs,
		OutputTableIDs: wf.OutputTableIDs,
		SecretIDs:      wf.SecretIDs,
		QuerySecretIDs: wf.QuerySecretIDs,
		AgentIDs:       wf.AgentIDs,
		CodexIDs:       wf.CodexIDs,
	}
}

// describeBindings renders a binding set as one line — "2 input tables, 1
// output table, 1 secret" — or "no data or secret bindings" when empty.
func describeBindings(b api.ValidateBindings) string {
	var parts []string
	for _, kind := range []struct {
		ids      []string
		singular string
	}{
		{b.InputTableIDs, "input table"},
		{b.OutputTableIDs, "output table"},
		{b.SecretIDs, "secret"},
		{b.QuerySecretIDs, "query secret"},
		{b.AgentIDs, "agent"},
		{b.CodexIDs, "codex"},
	} {
		if n := len(kind.ids); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, kind.singular)))
		}
	}
	if len(parts) == 0 {
		return "no data or secret bindings"
	}
	return strings.Join(parts, ", ")
}

// The width of a report's key column, which is not the same everywhere: push
// and publish align to "Draft:    ", while a status block is one word wider —
// "Entrypoint: " sets it. Passed to printResourceURL rather than duplicating
// its rule per column, since which column a report uses says nothing about
// when a link should be printed.
const (
	reportKeyWidth = 10
	statusKeyWidth = 12
)

// printResourceURL closes a report with the frontend page for what was just
// written, in the same key-value column as the ids above it — keyWidth being
// that column, one of the two constants above.
//
// The url is whatever the SERVER put on the response we already hold, and this
// is the only thing done with it — nothing here templates a route or picks an
// origin. That is the point rather than tidiness: the instance URL a profile
// records is the API origin, and the deploy template puts the backend on api.*
// and the frontend on app.*, so the link this replaces pointed at a host that
// serves no pages. (The server had the same bug from its own side; both were
// fixed by giving one package the mapping AND the origin, backend/lib/deeplink.)
//
// ABSENCE IS SILENT. An instance with no configured frontend origin returns no
// url — the normal state of a dev box, which is exactly where this gets tested
// — so there is nothing to say and nothing to warn about. No link, no note, no
// non-zero exit, and above all no locally-guessed fallback. Every report that
// prints a link comes through here, so that rule has exactly one home.
//
// io.Writer and not *os.File: every caller passes a command's out stream, and
// nothing here needs a file. The concrete type only narrowed who could call
// this — link_test.go already drives it through the command harness, but a
// direct assertion against a buffer should not have to open one.
func printResourceURL(out io.Writer, keyWidth int, url string) {
	if url == "" {
		return
	}
	fmt.Fprintf(out, "  %-*s%s\n", keyWidth, "URL:", url)
}

// agree picks a present-tense verb form to match a count, which is the OPPOSITE
// of plural's rule — a verb takes its "s" in the singular, a noun in the plural.
//
// Both forms are spelled out rather than derived, because the ones that need
// this are irregular ("is"/"are") as often as not, and a helper that only
// handled +"s" would be wrong exactly where somebody reached for it.
//
// ⚠️ The count is the SUBJECT's, which is not always the one the noun beside it
// agrees with: "1 of 1 folder makes" and "2 of 5 folders make" both read
// correctly, and they take their counts from different numbers.
func agree(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// plural pluralises the handful of binding nouns above. "codex" is the reason
// this is not a bare +"s".
func plural(n int, singular string) string {
	if n == 1 {
		return singular
	}
	if strings.HasSuffix(singular, "x") {
		return singular + "es"
	}
	return singular + "s"
}

// baselineFrom builds the state entry recorded after a successful sync with one
// workflow row.
//
// Title and Entrypoint are recorded from the ROW, never from the manifest: the
// baseline's job is to say what the server held at the last sync, and the drift
// guard compares the server against it. Recording what we WANTED it to be would
// make the guard agree with itself forever.
// baselineParameters records the row's declaration for the drift guard, always
// non-nil. "The server declared none" is a fact worth recording: a nil here
// means UNRECORDED, which disarms the guard, and that is only ever right for a
// state file written before parameters were part of the baseline.
func baselineParameters(source *api.Workflow) *[]api.WorkflowParameter {
	params := append([]api.WorkflowParameter{}, source.Parameters...)
	return &params
}

// baselineReportingTimezone records the row's declared execution calendar for
// the drift guard, always non-nil for the same reason baselineParameters is.
// "The server declares none" (an empty string, a row predating migration 000499)
// is a fact worth recording; a nil means UNRECORDED, which disarms the guard and
// is only ever right for a state file written before the zone was part of one.
func baselineReportingTimezone(source *api.Workflow) *string {
	zone := source.ReportingTimezone
	return &zone
}

// codec de-aliases the fetched content before it is hashed, for the reason
// hashFiles records: this baseline is compared against hashes of the bytes on
// disk, which are in alias form.
func baselineFrom(codec aliasCodec, source *api.Workflow, files []api.WorkflowFile) *wfdir.InstanceState {
	inst := &wfdir.InstanceState{
		SourceID:          source.ID,
		SourceLifecycle:   source.Lifecycle,
		BaselineUpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339),
		Title:             source.Title,
		Entrypoint:        source.Entrypoint,
		Parameters:        baselineParameters(source),
		ReportingTimezone: baselineReportingTimezone(source),
		Files:             map[string]wfdir.FileState{},
	}
	for _, f := range files {
		inst.Files[f.Path] = wfdir.FileState{
			SHA256:    wfdir.HashString(codec.toDisk(f.Content)),
			UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339),
		}
	}
	return inst
}

// baselineFromLocal is baselineFrom for content the CLI just WROTE rather than
// read back: after a push, the server holds exactly the bytes we sent.
//
// Re-fetching every file to build the baseline instead would be a second full
// round trip to learn something we already know.
//
// It takes NO codec, and that is the invariant rather than an omission: `files`
// is the folder's own content, already in disk form, and the baseline is a
// fingerprint of exactly that. Running toDisk over it would be de-aliasing
// something that was never aliased away. Per-file UpdatedAt is left
// empty — the field is diagnostics only (drift is decided on the hash), and
// inventing timestamps we did not receive would be worse than saying nothing.
func baselineFromLocal(source *api.Workflow, files map[string]string) *wfdir.InstanceState {
	inst := &wfdir.InstanceState{
		SourceID:          source.ID,
		SourceLifecycle:   source.Lifecycle,
		BaselineUpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339),
		Title:             source.Title,
		Entrypoint:        source.Entrypoint,
		Parameters:        baselineParameters(source),
		ReportingTimezone: baselineReportingTimezone(source),
		Files:             map[string]wfdir.FileState{},
	}
	for path, content := range files {
		inst.Files[path] = wfdir.FileState{SHA256: wfdir.HashString(content)}
	}
	return inst
}
