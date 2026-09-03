package wfdir

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/config"
)

// A STACK is one named environment this folder deploys to — "dev", "staging",
// "prod" — and it is the unit every command selects with --stack.
//
// The name is committed and SHARED, which is the whole reason it is not a
// profile name. InstanceKey already says this about the key it replaces: "It is
// deliberately NOT keyed by profile NAME. ronja.json is committed and shared,
// and a profile name is local to one machine — my 'prod' is not yours." A stack
// name is the opposite: it is the one word a team agrees on, and the credential
// that reaches it stays local. Two selectors, two jobs, no third: --stack names
// an ENVIRONMENT, --profile names a CREDENTIAL.
//
// What is in a Stack is CONFIG — decisions a human made, and what a pull request
// is about. Where it points (url, tenantID) and which feature its resources are
// created in (featureID) are all chosen once by a person. Everything a DEPLOY
// discovered — the workflow id it created, the tables it built, the fingerprint
// it last agreed with — is state and lives in ronja.lock.json instead. That
// split is Pulumi's discipline and it is why a CI push no longer dirties the
// file a reviewer reads.
//
// FeatureID is config and not state even though the first push "records" it,
// because what it records is the answer to a question a person answered: `wf
// init --feature <id>` is the human choosing where this workflow lives. The line
// is "who decided", not "who wrote the bytes".
type Stack struct {
	URL      string `json:"url"`
	TenantID string `json:"tenantID,omitempty"`
	// FeatureID is the container new resources are created in. Absent is legal —
	// it means nothing has chosen one yet, and the first push refuses with a
	// message naming this key.
	FeatureID string `json:"featureID,omitempty"`
	// Bind is this stack's answer to the folder's declared dependencies: alias →
	// the resource id THIS organization uses for it. See Dependency in
	// dependencies.go for what the alias layer is.
	//
	// CONFIG by the same test everything else here passes — which of this
	// organization's rows answers to `orders` is a decision a person makes, not
	// something a deploy discovered — so it is committed beside the stack in
	// ronja.json and never in ronja.lock.json.
	//
	// ⚠️ It lives on Stack and on Instance, and deliberately NOT on Binding.
	// Binding is the joined config+state value Manifest.Record writes back, and
	// callers build one from ids alone (`wf discard` hands in a fresh
	// Binding{FeatureID}). A bind map carried inside it would therefore be
	// CLEARED by the next push, out of a committed file, because a machine's
	// struct happened not to carry it. FeatureID above documents that exact
	// hazard and pays for it with an "empty does not clear" guard in Record; the
	// cheaper fix is the one taken here — keep config that no deploy ever writes
	// out of the value a deploy records. Selection.Bind is how a command reads
	// it.
	Bind map[string]string `json:"bind,omitempty"`

	// unknown is this stack's forward-compatibility sidecar, for the reasons on
	// Manifest.unknown. A stack entry is an object in a committed file, so it has
	// the identical hazard: a key a newer CLI writes here must survive an older
	// one rewriting the manifest around it.
	unknown unknownKeys
}

// Key is the (instance, organization) this stack points at.
func (s Stack) Key() InstanceKey { return InstanceKey{URL: s.URL, TenantID: s.TenantID} }

// Selection is which stack a command is acting on, with the two halves of the
// answer joined back together: the config a human wrote and the state a deploy
// recorded.
//
// Name is EMPTY for a legacy unnamed instances[] entry, and that empty string is
// not a stack called "" — it is the absence of the stack layer, which is what a
// folder written before stacks existed has.
//
// Two places branch on it and no others should: Manifest.Record, which sends a
// named selection's config to stacks and its state to the lock while an unnamed
// one goes wholly to instances[]; and the pipeline loop's liveHashes, which
// decides where that kind's LIVE fingerprints live. Everything else reads the
// joined Binding and never asks which shape it came from.
type Selection struct {
	Name    string
	Key     InstanceKey
	Binding Binding
	// Bound reports whether this folder HAS an entry for this place — a declared
	// stack, or an instances[] entry. It is not "something has been created
	// there": a stack declared with a featureID and nothing in the lock is bound
	// and has no workflow yet, exactly as a legacy entry carrying only a
	// featureID always has been. Unbound is the ordinary "never pushed there"
	// state, and is what the first push acts on.
	Bound bool
	// Bind is the selected stack's alias → resource id map — CONFIG, read-only,
	// and never written back by a push. It comes from Stack.Bind for a named
	// stack and from Instance.Bind for the legacy unnamed entry, joined here for
	// the reason everything else in Selection is: so a command reads one shape
	// and never asks which of the two it came from.
	//
	// Nil is the ordinary state and is not the same claim as "declares none": a
	// folder with no dependencies binds nothing, and so does a stack nobody has
	// bound yet. Manifest.CheckBind is what tells those apart, at the point a
	// command is about to act on one.
	Bind map[string]string
}

// Named reports whether this selection is a stack rather than the legacy
// unnamed entry.
func (s Selection) Named() bool { return s.Name != "" }

// ErrAmbiguousStack is returned when nothing says which stack a command means
// and more than one could be it. Distinct from ErrAmbiguousInstance, which is
// about ORGANIZATIONS on one instance: this one is reachable with a perfectly
// well-known organization, because two stacks may name two features in it.
var ErrAmbiguousStack = fmt.Errorf("this folder names several stacks that could be meant")

// maxStackNameLen caps a stack name. It is a word people type on a command
// line and read in a diff, not a description.
const maxStackNameLen = 40

// ValidStackName reports whether a name may be written into a committed
// manifest, and says why not when it may not.
//
// Deliberately narrow. The name appears in three places — a JSON object key, a
// --stack argument, and prose in an error message — so anything that needs
// quoting in a shell, or that two readers would spell differently, is refused at
// the point it is invented rather than discovered later by whoever types it.
func ValidStackName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a stack name cannot be empty")
	case len(name) > maxStackNameLen:
		return fmt.Errorf("stack name %q is %d characters, and the limit is %d — it is a word you type on a command line, not a description", name, len(name), maxStackNameLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("stack name %q may only hold letters, digits, dash, underscore and dot — it is a JSON key in a committed file and an argument you type, and %q is neither", name, string(r))
		}
	}
	for _, first := range name {
		// A range loop rather than name[0], which is a BYTE. It happens to be a
		// rune here only because the loop above has already refused everything
		// outside ASCII — so reordering the two checks would turn this into a
		// silent truncation, and a form that does not depend on that is worth the
		// two extra lines.
		if !isStackNameAlnum(first) {
			return fmt.Errorf("stack name %q must start with a letter or a digit", name)
		}
		break
	}
	return nil
}

func isStackNameAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// StackNames lists the declared stacks in a stable order, so every listing and
// every message reads the same way twice.
func (m *Manifest) StackNames() []string {
	out := make([]string, 0, len(m.Stacks))
	for name := range m.Stacks {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// UsesStacks reports whether this folder has moved to the stack layer. It is the
// one predicate that decides whether a NEW binding must be named, and therefore
// the thing that keeps a v2 folder from quietly growing a legacy entry beside
// its stacks.
func (m *Manifest) UsesStacks() bool { return len(m.Stacks) > 0 }

// entries is every binding this folder has — named stacks first, in name order,
// then the unnamed legacy remainder in file order.
//
// Joined here and nowhere else, so the two shapes cannot answer a lookup
// differently. The state half comes from the lock for a named stack and from the
// entry itself for a legacy one — the one place the BINDING's fork is spelled
// (the live-fingerprint fork has its own, in the pipeline loop's liveHashes).
func (m *Manifest) entries(lock *Lock) []Selection {
	out := make([]Selection, 0, len(m.Stacks)+len(m.Instances))
	for _, name := range m.StackNames() {
		stack := m.Stacks[name]
		binding := lock.Binding(name)
		binding.FeatureID = stack.FeatureID
		out = append(out, Selection{Name: name, Key: stack.Key(), Binding: binding, Bound: true, Bind: stack.Bind})
	}
	for _, inst := range m.Instances {
		out = append(out, Selection{Key: inst.Key(), Binding: inst.Binding, Bound: true, Bind: inst.Bind})
	}
	return out
}

// KeysOn lists every (instance, organization) this folder names on one instance,
// stacks and legacy entries alike, in a stable order.
//
// It covers BOTH shapes, and that is the point of it existing rather than each
// caller reaching for the free function. Reading only instances[] was correct
// while that was the only shape; against a stack folder it answers "nothing
// here", and the two callers act on that answer in ways that are wrong in
// opposite directions — one skips the organization lookup and then matches on
// URL alone, and the other lists no organizations in a refusal about which
// organization applies.
func (m *Manifest) KeysOn(url string) []InstanceKey {
	stacks := make([]Stack, 0, len(m.Stacks))
	for _, name := range m.StackNames() {
		stacks = append(stacks, m.Stacks[name])
	}
	return append(KeysOn(stacks, Stack.Key, url), KeysOn(m.Instances, Instance.Key, url)...)
}

// checkStacks refuses a manifest whose stacks are individually unusable.
//
// PER-STACK rules ONLY, and that boundary is itself the invariant: THE CLI MUST
// NEVER WRITE A MANIFEST IT WILL THEN REFUSE TO LOAD. A rule about a PAIR of
// stacks fails that test twice over. It can be created by something no writer
// ever saw — a git merge bringing two branches' stacks together is a clean
// textual merge — and enforced at LOAD it makes the folder unopenable by every
// command, including `status` and an explicit `--stack prod` that would resolve
// perfectly well. The only recovery would be hand-editing a committed file the
// CLI may have written itself.
//
// So the two cross-stack rules live where a name is ACCEPTED and where one is
// SELECTED instead, and both leave `--stack <name>` working:
//
//   - Two names differing only by case. They are two JSON keys and one word to
//     everybody reading them, so `--stack Prod` against a folder naming `prod`
//     would create a second set of resources rather than finding the first.
//     Refused by selectNamed, which is the only door a new name comes through.
//   - Two stacks pointing at the SAME (instance, organization). The selection
//     that runs when no --stack is given matches on exactly that pair, and the
//     local baseline in .ronja/state.json is keyed by it too — so a second stack
//     there shares one checkout's draft fingerprints with another environment's
//     rows. selectNamed refuses to CREATE the second one, and Select reports the
//     pair as an ambiguity naming both. Closing the limit properly means keying
//     the baseline by stack name; see BL-4e71.
func (m *Manifest) checkStacks(path string) error {
	for _, name := range m.StackNames() {
		if err := ValidStackName(name); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		stack := m.Stacks[name]
		if stack.TenantID == "" {
			// A stack is an (instance, ORGANIZATION), and one that names no
			// organization is unusable rather than merely incomplete: implicit
			// selection needs an exact organization match, and an explicit
			// --stack is refused against every credential that knows its own. It
			// is refused HERE because this is the only place that can name the
			// missing key — the selection refusals talk about picking a different
			// credential, which is the wrong advice entirely.
			return fmt.Errorf("%s declares stack %q with no \"tenantID\", and a stack is one organization on one instance. Add the organization's id to it — `ronja whoami` prints the one your credential reaches",
				path, name)
		}
	}
	return nil
}

// sharedBaselineWarning is the sentence every refusal about two entries on ONE
// (instance, organization) ends with. Written once because it is the same fact
// each time — the local baseline is keyed by that pair, not by stack name — and
// two spellings of it would drift.
const sharedBaselineWarning = "one folder tracks one deployment per organization: its local sync baseline is keyed by that pair, so two entries there compare this checkout's drafts against each other's rows — use a second folder for the second deployment"

// describeKey renders an (instance, organization) for a message.
func describeKey(key InstanceKey) string {
	if key.TenantID == "" {
		return key.URL
	}
	return key.TenantID + " on " + key.URL
}

// Select resolves which stack a command acts on, from the credential's own
// (instance, organization) and an optional explicit --stack name.
//
// The two selectors do not compete: an explicit name WINS and is checked against
// the credential, and the implicit match is what every folder written before
// stacks existed keeps getting. The rules, and each one is a refusal somebody
// would otherwise have discovered as a duplicate resource:
//
//   - --stack names a DECLARED stack. It must point where this credential
//     reaches, or the push would send this folder's files to the named stack's
//     feature under the wrong organization's token.
//   - --stack names an UNDECLARED stack, and this (instance, organization)
//     already has one under a different name. Refused: that is a typo, and the
//     message names the stack that was meant. Without it `--stack prd` creates a
//     second workflow beside `prod` and reports success.
//   - --stack names an UNDECLARED stack somewhere this folder has never been
//     pushed. That is the ordinary way a stack is born, and it is also the only
//     way a legacy folder migrates: the human supplied the name, which is the
//     one thing an instances[] entry does not have.
//   - No --stack, and the LOCK names stacks the manifest does not. Refused:
//     that is the stranded half of a lock-first write, and the implicit path
//     cannot see it. See strandedLockStacks.
//   - No --stack: match on (url, tenantID) exactly as the folder always did.
//     One match is the answer; several need a name; none means "never pushed
//     here", which is a first push.
func (m *Manifest) Select(lock *Lock, key InstanceKey, want string) (Selection, error) {
	if want != "" {
		return m.selectNamed(lock, key, want)
	}
	if err := m.refuseStrandedLockStacks(lock); err != nil {
		return Selection{Key: key}, err
	}
	entries := m.entries(lock)
	matches := FindAll(entries, Selection.selectionKey, key)
	switch {
	case len(matches) == 1:
		return entries[matches[0]], nil
	case len(matches) == 0:
		// Never pushed here. An ordinary state, and the first push acts on it.
		return Selection{Key: key}, nil
	case !key.Known():
		// Several entries on this URL and nothing says which organization. This
		// is ErrAmbiguousInstance and stays that way: the caller's refusal names
		// the organizations and how to pick one, and a stack name is now a second
		// way to do it.
		return Selection{Key: key}, ErrAmbiguousInstance
	default:
		// A KNOWN organization matching several entries: two stacks a merge
		// brought together, or a hand-edited file where a legacy entry and a
		// stack both claim one place. REPORTED rather than refused at load, so
		// naming one with --stack still works — see checkStacks.
		return Selection{Key: key}, fmt.Errorf("%w for %s — name one with --stack (%s). Note that %s",
			ErrAmbiguousStack, describeKey(key), strings.Join(namesOf(entries, matches), ", "),
			sharedBaselineWarning)
	}
}

// refuseStrandedLockStacks closes the other half of SaveFolder's lock-first
// ordering, on the door that ordering was never covered on.
//
// SaveFolder writes ronja.lock.json BEFORE ronja.json precisely so that a crash
// between the two leaves the created ids RECOVERABLE: selectNamed treats a lock
// entry under the requested name as bound and adopts it. But that recovery only
// exists behind an explicit --stack. A first-ever `ronja wf push --stack dev`
// whose lock write lands and whose manifest write fails leaves a lock naming
// "dev" with a real workflow id and a manifest naming no stack at all — and the
// implicit path reads the folder through m.entries(lock), which walks
// m.Stacks. It sees nothing. The next FLAGLESS push therefore takes the create
// path and makes exactly the duplicate the lock-first ordering exists to
// prevent.
//
// The fix is a REFUSAL, not a second recovery. Recovering here would mean
// guessing which of the lock's stacks this flagless command meant, and a wrong
// guess pushes one environment's files into another's — worse than the
// duplicate. The reader has the one piece of information that resolves it (the
// name), so the message asks for it.
//
// Deliberately narrow, so the two legitimate shapes are untouched: a folder with
// NO lock file at all (LoadLock answers an empty Lock), and a v1 folder that has
// never used stacks (SaveLock writes nothing for it, so its lock stays absent).
// Both have len(lock.Stacks) == 0 and never reach the refusal.
func (m *Manifest) refuseStrandedLockStacks(lock *Lock) error {
	if m.UsesStacks() || lock == nil || len(lock.Stacks) == 0 {
		return nil
	}
	names := make([]string, 0, len(lock.Stacks))
	for name := range lock.Stacks {
		names = append(names, name)
	}
	sort.Strings(names)
	noun := "stacks"
	if len(names) == 1 {
		noun = "stack"
	}
	return fmt.Errorf("%s records %s (%s) and %s names none — the ids recorded there were created by a push whose lock write landed and whose manifest write did not. Pushing with no --stack cannot see them, and would create a second set of resources beside them. Name one with --stack (%s): that adopts what is already there and repairs %s",
		LockName, noun, strings.Join(names, ", "), ManifestName,
		strings.Join(names, ", "), ManifestName)
}

// namesOf renders the matched entries for an ambiguity message, naming the
// unnamed legacy entry as what it is rather than as an empty string.
func namesOf(entries []Selection, matches []int) []string {
	out := make([]string, 0, len(matches))
	for _, i := range matches {
		if entries[i].Named() {
			out = append(out, entries[i].Name)
			continue
		}
		out = append(out, `the unnamed "instances" entry`)
	}
	return out
}

// selectionKey is Selection's InstanceKey, as a method value FindAll can take.
func (s Selection) selectionKey() InstanceKey { return s.Key }

func (m *Manifest) selectNamed(lock *Lock, key InstanceKey, want string) (Selection, error) {
	if err := ValidStackName(want); err != nil {
		return Selection{}, err
	}
	if stack, ok := m.Stacks[want]; ok {
		declared := stack.Key()
		// TWO checks, not one, and splitting them is load-bearing.
		//
		// The INSTANCE is always known — it is where the credential points — so it
		// is compared unconditionally. A single Matches() covering both halves
		// skips this whole comparison for a credential with no organization, which
		// is exactly what a $RONJA_TOKEN credential is: `--stack prod` under a
		// localhost token would then adopt prod's featureID and push to localhost
		// with it. That is the wrong-place write in its worst form, on the one
		// credential path CI is documented to use.
		//
		// The ORGANIZATION is compared only when it is known. An unresolved one is
		// not a mismatch — it is the signed-out `status` case — and the declared
		// stack is a better answer there than nothing.
		if !Matches(InstanceKey{URL: declared.URL}, InstanceKey{URL: key.URL}) {
			return Selection{}, fmt.Errorf("stack %q is on %s, and this credential reaches %s. Pick the credential that matches (--profile <name>, or --url), or the stack that matches this credential",
				want, declared.URL, key.URL)
		}
		if key.Known() && declared.TenantID != key.TenantID {
			return Selection{}, fmt.Errorf("stack %q is %s, and this credential reaches %s. Pick the credential that matches (--profile <name>), or the stack that matches this credential",
				want, describeKey(declared), describeKey(key))
		}
		binding := lock.Binding(want)
		binding.FeatureID = stack.FeatureID
		return Selection{Name: want, Key: declared, Binding: binding, Bound: true, Bind: stack.Bind}, nil
	}
	// Undeclared, which means this call may be about to INVENT the name — so it
	// is the door every cross-stack rule is enforced at. checkStacks no longer
	// applies them at load, because a pair of stacks can arrive by a git merge
	// and a load-time refusal makes the folder unopenable.
	//
	// Capitalisation first. Two keys differing only by case are one word to
	// everybody reading them, and writing the second is how `--stack Prod`
	// against a folder naming `prod` ends up creating a second set of resources
	// and reporting success.
	for _, name := range m.StackNames() {
		if strings.EqualFold(name, want) {
			return Selection{}, fmt.Errorf("this folder names stack %q, and %q differs from it only by capitalisation — they are one word to everybody reading them, and a second key would be a second set of resources. Use --stack %s",
				name, want, name)
		}
	}
	// A lock entry under exactly this name and no stack declaring it: the ids
	// under it were created by a push whose lock write landed and whose manifest
	// write did not. BOUND, so the folder adopts them — the alternative is the
	// duplicate-creation this whole file exists to stop, and it is what made
	// SaveFolder's lock-first ordering safe only halfway.
	//
	// The KEY is the credential's, since the lock records no url or organization
	// of its own. A stale entry left by a hand-edit that pointed somewhere else
	// therefore hands over an id this credential cannot see, and the push paths
	// report that as a broken binding rather than creating a replacement — a
	// legible failure, where creating a second resource is not.
	if _, recorded := lock.entry(want); recorded {
		binding := lock.Binding(want)
		return Selection{Name: want, Key: key, Binding: binding, Bound: true}, nil
	}
	// If this (instance, organization) is already named, the name given is a
	// typo — creating a second set of resources beside the first is the one
	// outcome nothing downstream can undo.
	if key.Known() {
		for _, name := range m.StackNames() {
			if Matches(m.Stacks[name].Key(), key) {
				return Selection{}, fmt.Errorf("this folder already calls %s %q, and there is no stack %q. Use --stack %s, or add a %q entry to \"stacks\" in %s first if you really mean a second deployment — though %s",
					describeKey(key), name, want, name, want, ManifestName, sharedBaselineWarning)
			}
		}
		for _, inst := range m.Instances {
			if Matches(inst.Key(), key) {
				// The migration path: a legacy entry, and a human has now supplied
				// the one thing it never had — a name. Bound is TRUE and the
				// binding comes from the entry, so the first write moves it across
				// rather than creating anything — its bind map included, which
				// Record then carries onto the stack it becomes.
				return Selection{Name: want, Key: inst.Key(), Binding: inst.Binding, Bound: true, Bind: inst.Bind}, nil
			}
		}
	}
	return Selection{Name: want, Key: key}, nil
}

// Record writes a resolved binding back, splitting it the way the format does:
// the half a human decided into ronja.json, the half a deploy discovered into
// ronja.lock.json. A legacy unnamed selection keeps both in instances[], exactly
// as it always has — a v1 folder that nobody has named a stack in is not
// rewritten by a push.
//
// It also completes the migration: naming a legacy entry moves it into stacks
// and DROPS the instances[] entry, so the two shapes never both answer for one
// (instance, organization).
//
// ⚠️ A NAMED selection must carry a known organization. checkStacks refuses a
// stack with no "tenantID", so recording one here writes a manifest the very
// next command cannot open — the one thing this package must never do. It is
// not enforced here because there is no error to return; the two write doors
// guard it instead, and both say so: recordFirstBindingInto refuses outright,
// and adoptStack declines silently because the command it sits in is working
// fine and the migration is an offer rather than the job.
func (m *Manifest) Record(lock *Lock, sel Selection, b Binding) {
	if !sel.Named() {
		m.SetBinding(sel.Key, b)
		return
	}
	url := sel.Key.URL
	if norm, err := config.NormalizeURL(url); err == nil {
		url = norm
	}
	stack := m.Stacks[sel.Name]
	stack.URL, stack.TenantID = url, sel.Key.TenantID
	if b.FeatureID != "" {
		// An EMPTY featureID does not clear the declared one. The binding
		// travelling through here is sometimes state-shaped — built by a caller
		// that only had ids to record — and a stack's featureID is CONFIG, the
		// answer a person gave to `init --feature <id>`. Wiping a human's line
		// out of a committed file because a machine's struct happened not to
		// carry it is the one direction this split must never go.
		stack.FeatureID = b.FeatureID
	}
	if m.Stacks == nil {
		m.Stacks = map[string]Stack{}
	}
	// Any legacy entry for the same place has been superseded. Left in place it
	// would keep answering an implicit lookup made by an older command path, and
	// the folder would have two bindings for one organization.
	kept := m.Instances[:0]
	for _, existing := range m.Instances {
		if !Matches(existing.Key(), sel.Key) {
			kept = append(kept, existing)
			continue
		}
		// The entry being migrated may carry keys this build has no field for,
		// which SetBinding has always carried across when it REPLACED an entry.
		// A migration is the same move by another name, so they come with it —
		// dropping them here would make naming a stack the one rewrite that
		// silently loses what a newer CLI wrote, which is exactly what the
		// sidecars exist to stop. Only onto a stack that captured nothing of its
		// own: a declared stack's own recorded order and keys are the better
		// answer for it.
		if len(stack.unknown.order) == 0 && len(stack.unknown.values) == 0 {
			stack.unknown = existing.unknown
		}
		// The entry's alias bindings migrate with it, for the same reason: they
		// are config a person wrote about this (instance, organization), and
		// naming a stack must not be the one rewrite that silently drops them.
		// Only onto a stack that has none of its own — a declared stack's own
		// bind map is the better answer for it, and the two are never merged,
		// since a half-merged map is a binding nobody wrote.
		if len(stack.Bind) == 0 {
			stack.Bind = existing.Bind
		}
	}
	m.Instances = kept
	m.Stacks[sel.Name] = stack
	lock.SetBinding(sel.Name, b)
}
