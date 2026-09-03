package wfdir

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/markers"
)

// A DEPENDENCY is a name this folder's CODE uses for a row it does not own, and
// it is what makes a folder deployable to more than one organization.
//
// The problem it solves: a resource id belongs to exactly one organization. A
// folder whose SQL says `{{ ref('table-abc') }}` is a folder that can only ever
// be pushed to the organization that minted table-abc — cloned into another and
// pushed, it builds against an id nobody there has, and the refusal it earns
// names an id that means nothing to the reader. So the code says
// `{{ ref('orders') }}` instead, ronja.json declares `orders` as a dependency of
// kind `table`, and each stack binds that name to the id its own organization
// uses.
//
// The alias namespace is owned by the FOLDER, and resolution happens in the CLI
// before anything is sent. The server's authorization surface is unchanged by
// all of this: it only ever receives ids, exactly as it did before, because a
// second grammar on that surface would be a second place to get access checks
// right.
//
// Two halves, in two places, on the split this package draws everywhere:
// Manifest.Dependencies is CONFIG — what a person decided, and what a pull
// request is about — and so is Stack.Bind, because choosing which of this
// organization's rows answers to `orders` is a decision, not something a deploy
// discovered. Neither is ever written by a push.
type Dependency struct {
	// Kind is which primitive the alias names — one of markers.Kinds(). It is
	// declared rather than inferred from the code, because the marker family is
	// what would have to infer it, and a name used by no marker yet (a
	// dependency added before the code that reads it) would then have no kind at
	// all.
	Kind string `json:"kind"`

	// unknown is this entry's forward-compatibility sidecar, for the reasons on
	// Manifest.unknown. A dependency is an object in a COMMITTED file, so a key
	// a newer CLI writes into it — a description, a default, a required flag —
	// has to survive an older one rewriting the manifest around it. Every
	// nesting level of both committed files carries one; a level that does not
	// is silent, and what it drops is what a colleague's newer CLI wrote.
	unknown unknownKeys
}

// dependencyKeys is the set of object keys a Dependency marshals to, so the
// sidecar can tell "a key this CLI owns" from "a key from the future".
var dependencyKeys = jsonFieldNames(reflect.TypeOf(Dependency{}))

// UnmarshalJSON / MarshalJSON keep the keys this build has no field for, for the
// reasons on Manifest's pair. plainDependency is the method-less twin that stops
// the recursion; see forwardcompat.go.
func (d *Dependency) UnmarshalJSON(data []byte) error {
	var decoded plainDependency
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*d = Dependency(decoded)
	d.unknown.capture(data, dependencyKeys)
	return nil
}

// MarshalJSON writes the struct's own keys and puts the unrecognised ones back.
//
// A VALUE receiver, so a Dependency inside a map — which is never addressable —
// is marshalled through this method too. A pointer receiver would silently skip
// every entry in Manifest.Dependencies, which is the only place these live.
func (d Dependency) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(plainDependency(d))
	if err != nil {
		return nil, err
	}
	return d.unknown.merge(body)
}

// maxAliasNameLen caps an alias. It is a JSON key in a committed file, a string
// inside a quoted marker argument, and a word in an error message — longer than
// this and it is documentation, which belongs in a key of its own.
const maxAliasNameLen = 64

// ValidAliasName reports whether a name may be declared as a dependency, and
// says why not when it may not.
//
// Narrow, for the reasons ValidStackName is, plus two rules that exist only
// here and are each a silent wrong-deploy if they are missing:
//
//   - ALL DIGITS is refused because `{{ ref('0') }}` is already a grammar: a
//     POSITIONAL ref, an index into the row's input_models, and the form the AI
//     build path persists (see the tablerefs package doc). An alias called "0"
//     would be a name that reads as an index to the resolver and as an index to
//     the server, and the table it landed on would depend on which of them
//     looked first. markers.Occurrence.Positional is the other half of this
//     guarantee; neither half is safe alone.
//
//   - ID-SHAPED is refused for every MARKER kind, not only the one declared. An
//     alias called `table-orders` resolves to itself — it is already id-shaped,
//     so every reader downstream, here and on the server, takes it for a literal
//     id and looks for a row nobody has. Checking every kind rather than the
//     declared one costs nothing and closes the case where a `secret` dependency
//     is named `table-x`.
//
//     ⚠️ markers.MarkerKinds(), NOT markers.Kinds(), and the narrowing is the
//     whole point. This function runs at LOAD, over manifests committed before
//     this build existed. A kind that joins the legal set LATER brings its id
//     prefixes with it, and scanning those here would retroactively refuse a
//     name that was legal when it was written: `note` arrived carrying `note-`
//     and `skill-`, so an existing workflow folder with an alias called
//     `note-taking` would stop opening — for every command, `status` included,
//     with hand-editing a committed file as the only recovery. That is verbatim
//     the failure checkStacks forbids: THE CLI MUST NEVER WRITE A MANIFEST IT
//     WILL THEN REFUSE TO LOAD. The marker-less half of this rule is real and is
//     enforced at the acceptance point instead — see CheckDeclarations.
func ValidAliasName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a dependency alias cannot be empty")
	case len(name) > maxAliasNameLen:
		return fmt.Errorf("alias %q is %d characters, and the limit is %d — it is a name you write inside a marker, not a description", name, len(name), maxAliasNameLen)
	}
	digits := true
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			digits = false
		case r == '-', r == '_', r == '.':
			digits = false
		default:
			return fmt.Errorf("alias %q may only hold letters, digits, dash, underscore and dot — it is a JSON key in a committed file and a name inside a quoted marker argument, and %q is neither", name, string(r))
		}
	}
	for _, first := range name {
		// A range loop rather than name[0], which is a BYTE — the same reasoning
		// ValidStackName records: it is a rune here only because the loop above
		// has already refused everything outside ASCII, and a form that does not
		// depend on the order of two checks is worth two lines.
		if !isStackNameAlnum(first) {
			return fmt.Errorf("alias %q must start with a letter or a digit", name)
		}
		break
	}
	if digits {
		return fmt.Errorf("alias %q is all digits, and `{{ ref('%s') }}` already means something else: an INDEX into the table's inputs, which is the form the web app writes. Give it a name with a letter in it", name, name)
	}
	for _, kind := range markers.MarkerKinds() {
		if err := refuseIDShapedAlias(name, kind); err != nil {
			return err
		}
	}
	return nil
}

// refuseIDShapedAlias is the one spelling of the id-shape refusal, so the load
// half (ValidAliasName) and the acceptance half (CheckDeclarations) cannot come
// to describe the same mistake two ways.
func refuseIDShapedAlias(name, kind string) error {
	if !markers.IsResourceID(kind, name) {
		return nil
	}
	return fmt.Errorf("alias %q is shaped like a %s id, so everything downstream — this CLI and the server — reads it as one and looks for a row with that id. Give it a name that is not %q-prefixed", name, kind, strings.SplitAfter(name, "-")[0])
}

// AliasNames lists the declared aliases in a stable order, so every listing and
// every message reads the same way twice.
func (m *Manifest) AliasNames() []string {
	out := make([]string, 0, len(m.Dependencies))
	for name := range m.Dependencies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// checkDependencies refuses a manifest whose declared dependencies are
// individually unusable, and stops exactly there.
//
// PER-DEPENDENCY rules ONLY, on checkStacks's discipline and for its stated
// invariant: THE CLI MUST NEVER WRITE A MANIFEST IT WILL THEN REFUSE TO LOAD.
// A rule spanning a dependency and a stack's bind map fails that test twice
// over. It can be produced by something no writer ever saw — a git merge that
// takes one branch's `dependencies` and another's `bind` is a clean textual
// merge — and it can be produced by the CLI itself, since removing a dependency
// leaves every stack's bind for it behind. Enforced at LOAD, either would make
// the folder unopenable by every command, `status` included, with hand-editing
// a committed file as the only recovery.
//
// So the cross-object rules live at ACCEPTANCE instead — see CheckBind, which a
// command calls once it knows which stack it is about to push,
// CheckAliasCollisions, which runs where a name is invented, and
// CheckDeclarations, which holds the two declaration rules that FAIL THE SAME
// TEST for a different reason: this build cannot write either of them, so only a
// newer CLI or a hand-edit produces one, and refusing at load would make a
// folder somebody else wrote unopenable rather than un-pushable.
//
// The two rules that ARE here are per-object, and this build can and does write
// both: a name is valid or it is not, and a dependency has a "kind" or it has
// none. Neither can be merged into existence, and neither describes a file a
// newer CLI would legitimately produce — adding a dependency key without a kind
// would change what an existing key MEANS, which is a format-version move, and
// the version gate refuses that file before any of this runs.
//
// ⚠️ ONE PIECE OF THE NAME RULE IS DELIBERATELY NOT HERE. ValidAliasName scans
// the MARKER kinds only for an id-shaped alias, because the set of kinds GROWS:
// scanning a newly added kind's prefixes at load would refuse a name that was
// legal in every build that came before, on a file already committed. That is
// the same test the paragraph above applies, failed for a third reason — not
// "somebody else wrote it" but "an earlier version of us did". The marker-less
// half lives in CheckDeclarations.
func (m *Manifest) checkDependencies(path string) error {
	for _, alias := range m.AliasNames() {
		if err := ValidAliasName(alias); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if kind := m.Dependencies[alias].Kind; kind == "" {
			return fmt.Errorf("%s declares dependency %q with no \"kind\", and the kind is what says which marker resolves it. Add one of: %s",
				path, alias, strings.Join(markers.Kinds(), ", "))
		}
	}
	return nil
}

// CheckDeclarations refuses a manifest whose DECLARATIONS this build cannot act
// on, at the ACCEPTANCE point rather than at load.
//
// Every rule here describes a file A REFUSAL AT LOAD WOULD STRAND, which is what
// puts them beside CheckBind rather than in checkDependencies: a rule enforced
// at load makes the folder unopenable by every command, `status` included, and
// the author of a file this build cannot act on is exactly the person who needs
// `status` to explain it. See checkStacks for the invariant, and
// checkDependencies for the half that stays at load.
//
// The first two describe a file THIS BUILD NEVER WRITES. The third is the one
// that widened the boundary: this build DOES write it, and an older build wrote
// it legitimately — the file is stranded by us adding a kind, not by somebody
// else's CLI. Same conclusion either way, which is why it belongs here.
//
// The three refusals:
//
//   - DEPENDENCIES DECLARED BELOW THE VERSION THAT MEANS THEM. `dependencies` is
//     a v3 key and MarshalJSON stamps 3 on any manifest that carries one, so
//     this build cannot produce the pair; a hand-authored or hand-edited file
//     can. It matters because the pair is a LIE that the version gate is
//     powerless against: the file MEANS v3 — its markers hold names, and the ids
//     exist nowhere in the source — while DECLARING that a v1 or v2 CLI may read
//     it. That colleague's CLI passes the gate, resolves nothing, and sends
//     `acme_api` to the server as a literal secret id, which the secret families
//     SOFT-FAIL into a warning rather than refusing. Green push, workflow
//     deployed with nothing bound, failure at run time in an organization the
//     pusher may not be watching. See ManifestFormatVersion, which exists for
//     precisely this failure and can only stop it if the file says so.
//   - AN UNKNOWN KIND. Refused rather than ignored because ignoring it is the
//     failure this whole layer exists to stop: the resolver would leave that
//     alias in the source and the server would read the literal name as an id,
//     which for a secret marker is the same soft-failing warning again. It
//     cannot live at load for the reason above AND for one of its own — the
//     format version cannot catch a new kind, since adding one adds no key, so a
//     folder written by a newer CLI carries a kind this build has never heard of
//     with no version to warn about it. Refusing that at load would take away
//     `status` from the one person best placed to see what happened.
//   - AN ALIAS DECLARED AS A MARKER-LESS KIND AND SHAPED LIKE THAT KIND'S ID.
//     The rule itself is ValidAliasName's — an id-shaped alias resolves to
//     itself, and every reader downstream reads it as a literal id — and for the
//     MARKER kinds it stays there, at load, where it has always been. It cannot
//     follow them for a kind added LATER: `note` brought the `note-` and
//     `skill-` prefixes into the legal set on the branch that added automations,
//     and scanning them at load would refuse a `note` alias called `note-x` in a
//     manifest that opened yesterday, leaving hand-editing a committed file as
//     the only recovery. Refused here instead, that folder still opens, still
//     reports, and is refused at the push — the one moment the bad name would
//     actually deploy something unresolvable. ⚠️ Scoped to the alias's OWN
//     declared kind, unlike the marker half — see the loop below for why
//     widening it would break folders nobody touched while closing nothing.
func (m *Manifest) CheckDeclarations() error {
	if len(m.Dependencies) > 0 && m.FormatVersion < manifestVersionDependencies {
		// FormatVersion is the DECLARED one — 0 is an absent key, and absent
		// means 1, so every version this build understands compares correctly
		// without normalising anything.
		return fmt.Errorf("%s declares \"dependencies\" but says \"formatVersion\": %d — an alias only resolves in a CLI that reads version %d, and a %s that claims less will be opened by one that does not resolve it, send your alias to the server as a literal id, and deploy this folder with nothing bound. Set \"formatVersion\": %d in %s",
			ManifestName, declaredFormatVersion(m.FormatVersion), manifestVersionDependencies,
			ManifestName, manifestVersionDependencies, ManifestName)
	}
	for _, alias := range m.AliasNames() {
		kind := m.Dependencies[alias].Kind
		if kind != "" && !isDependencyKind(kind) {
			// Names the possibility that this build is simply older than the
			// file, because that is the likelier cause than a typo once the set
			// has grown once.
			return fmt.Errorf("%s declares dependency %q with kind %q, and this ronja resolves only: %s. Check the spelling, or upgrade the CLI if the kind is newer than this build (`ronja --version` reports what you are running)",
				ManifestName, alias, kind, strings.Join(markers.Kinds(), ", "))
		}
		// The marker-less half of ValidAliasName's id-shape rule, scoped to the
		// alias's OWN declared kind — which is where it differs from the marker
		// half, and deliberately.
		//
		// ⚠️ Testing every alias against every marker-less kind would be
		// RETROACTIVE for no gain. `note` brought the `note-` and `skill-`
		// prefixes into the legal set on the branch that added automations, so a
		// workflow folder committed months ago whose `table` alias is called
		// `note-taking` would stop pushing on an edit nobody made. And it closes
		// nothing: every resolution path is keyed by (kind, name) — aliasCodec's
		// resolve and rewrite both look the alias up under the MARKER's own kind
		// — so that name resolves exactly as it always did. What genuinely does
		// not resolve is a `note` alias named `note-x`, which every reader
		// downstream reads as a literal id. That is the case kept.
		if isMarkerlessKind(kind) {
			if err := refuseIDShapedAlias(alias, kind); err != nil {
				return fmt.Errorf("%s: %w", ManifestName, err)
			}
		}
	}
	return nil
}

// declaredFormatVersion renders a manifest's FormatVersion the way a message
// should say it: an absent key is version 1, and reporting the raw 0 would name
// a version that has never existed.
func declaredFormatVersion(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// isMarkerlessKind reports whether a declared kind has no marker family behind
// it, which is what puts its id-shape check at acceptance rather than at load.
func isMarkerlessKind(kind string) bool {
	for _, markerless := range markers.MarkerlessKinds() {
		if kind == markerless {
			return true
		}
	}
	return false
}

// isDependencyKind reports whether a declared kind is one this build resolves.
func isDependencyKind(kind string) bool {
	for _, known := range markers.Kinds() {
		if kind == known {
			return true
		}
	}
	return false
}

// CheckBind refuses a selected stack whose bind map cannot be acted on.
//
// The ACCEPTANCE-point half of the validation checkDependencies deliberately
// stops short of, for the reason that function records: every rule here spans
// two objects, so a git merge or an ordinary edit can produce it, and refusing
// at load would make the folder unopenable rather than un-pushable. Called once
// a command knows which stack it is about to write through, where the refusal
// costs the reader one push and not their whole folder.
//
// The three refusals:
//
//   - A BIND WITH NO DECLARATION. Inert on its own — nothing resolves through
//     it, since resolution goes alias → bind and there is no alias — which is
//     exactly why it has to be said out loud: it is what a typo looks like
//     (`orders` declared, `order` bound), and left silent the reader sees only
//     the other half, "orders has no binding here", and adds a second entry
//     beside the one they meant to fix.
//   - A BIND VALUE THAT IS NOT ID-SHAPED for its kind. The value is what gets
//     substituted into the customer's committed source and sent as a real id;
//     a name there means the folder deploys with an unresolvable reference,
//     which for a secret marker is a warning on the server and a run-time
//     failure here.
//   - TWO ALIASES ON ONE ID. Blueprints refuse the same shape for the same
//     reason (rblueprint's ambiguous-bindings check), and there is a second one
//     here: it makes the id → alias direction ambiguous, which is what any
//     reverse mapping — reading a live row back into a folder, explaining which
//     declaration a resource answers — has to be able to do.
//
// A folder that declares nothing and binds nothing passes trivially, which is
// every folder that exists today.
func (m *Manifest) CheckBind(sel Selection) error {
	where := "the unnamed \"instances\" entry"
	if sel.Named() {
		where = fmt.Sprintf("stack %q", sel.Name)
	}
	// Sorted, so two runs against one broken folder name the same alias first.
	aliases := make([]string, 0, len(sel.Bind))
	for alias := range sel.Bind {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	owner := make(map[string]string, len(sel.Bind))
	for _, alias := range aliases {
		id := sel.Bind[alias]
		dep, declared := m.Dependencies[alias]
		if !declared {
			declaredNames := "none are declared"
			if names := m.AliasNames(); len(names) > 0 {
				declaredNames = "declared: " + strings.Join(names, ", ")
			}
			return fmt.Errorf("%s binds %q, and %s declares no dependency by that name — a bind is the answer to a declaration, so this one resolves nothing (%s). Add it to \"dependencies\", or fix the spelling",
				where, alias, ManifestName, declaredNames)
		}
		if !isDependencyKind(dep.Kind) {
			// A kind this build has no id prefixes for. IsResourceID answers
			// false for every string here, so the next check would refuse a
			// perfectly good id with "which is not a widget id" — a second
			// message about the one line CheckDeclarations has already named
			// properly, and the wrong one to act on. Deferred rather than
			// duplicated.
			continue
		}
		if !markers.IsResourceID(dep.Kind, id) {
			return fmt.Errorf("%s binds %q to %q, which is not a %s id — a bind names the row in THIS organization that the alias resolves to, and the value is substituted into the source and sent as an id. Use the id the web app shows for the %s",
				where, alias, id, dep.Kind, dep.Kind)
		}
		if first, dup := owner[id]; dup {
			return fmt.Errorf("%s binds both %q and %q to %s. Two names for one row leave nothing able to say which declaration that row answers to — bind one of them, and use that one name in the code",
				where, first, alias, id)
		}
		owner[id] = alias
	}
	return nil
}

// CheckAliasCollisions refuses a declared alias that collides with the stem of a
// sibling file in the same folder.
//
// A pipeline folder resolves `{{ ref('orders') }}` against a sibling
// `orders.sql` FIRST — the file you can see wins over a declaration you have to
// scroll for — so a dependency declared under the same name is dead text that
// reads as if it were in force. Someone binds it in every stack, reviews the
// diff, and the code goes on reading the local file. It is refused at the point
// the name is INVENTED rather than shadowed at the point it is used, because
// shadowing is only visible to whoever already suspects it.
//
// stems are the local names a folder's own files claim — for a pipeline, each
// .sql file's stem, which is exactly what tableNameFor derives.
//
// CASE-INSENSITIVE, and the fold is the load-bearing half rather than a
// courtesy. `ronja bind` answers a declaration with strings.EqualFold against a
// hit's title — it has to, because the search endpoint's own exact-match test is
// ILIKE — so a folder holding `Orders.sql` and declaring `orders` gets that
// declaration bound to the very row `Orders.sql` created. One word then names two
// things that are the same row, spelled two ways, with a `bind` entry in the
// committed file asserting they are different: exactly the ambiguity this
// refusal exists for, arrived at through a capital letter. Comparing byte for
// byte would wave it through.
//
// Exported because it is enforced from the commands layer — checkAliases calls
// it at the same acceptance point as CheckBind, since the stems it needs are the
// folder's files and this package never reads those.
func CheckAliasCollisions(deps map[string]Dependency, stems []string) error {
	if len(deps) == 0 || len(stems) == 0 {
		return nil
	}
	// Folded key → the stem as it is actually spelled on disk, so the message can
	// name the file the reader has to go and look at rather than a lowercased
	// version of it.
	claimed := make(map[string]string, len(stems))
	for _, stem := range stems {
		if folded := strings.ToLower(stem); claimed[folded] == "" {
			claimed[folded] = stem
		}
	}
	names := make([]string, 0, len(deps))
	for alias := range deps {
		names = append(names, alias)
	}
	sort.Strings(names)
	for _, alias := range names {
		stem, taken := claimed[strings.ToLower(alias)]
		if !taken {
			continue
		}
		if stem == alias {
			return fmt.Errorf("this folder declares a dependency called %q and also holds a file called %q — the file wins, so the declaration would never resolve anything. Rename one of them",
				alias, alias)
		}
		return fmt.Errorf("this folder declares a dependency called %q and also holds a file called %q — names are matched without regard to case when a declaration is bound, so the two would answer to one row while %s says they are different. Rename one of them",
			alias, stem, ManifestName)
	}
	return nil
}
