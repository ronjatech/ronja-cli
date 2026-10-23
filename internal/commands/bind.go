package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja bind` fills in a stack's answers to the folder's DECLARED dependencies.
//
// It is a SYNC verb, and the distinction is the one the CLI's whole design rule
// turns on. It never asks "what tables exist" — it asks, of each name this
// folder already committed to `dependencies`, "which row in THIS organization
// is called that". A folder is what supplies the questions; the organization
// supplies the answers; the result is a `bind` map a person reviews and commits.
// There is deliberately no way to reach it without a declaration to reconcile,
// which is what keeps it from becoming the `ronja list` this CLI does not have.
//
// It exists because the alternative is doing it by hand. Deploying an existing
// folder to a second organization means filling in a new stack's `bind` — half a
// dozen ids found in the web app and pasted into JSON — and a single transposed
// character there is a folder that deploys against the wrong table and reports
// success.
//
// KIND-AGNOSTIC, unlike every other folder command: `dependencies` is a manifest
// key all three folder kinds carry, so this is registered at the root rather
// than inside `wf` / `app` / `pipeline`, and it works in whichever folder it is
// run from.
//
// Four rules it does not bend:
//
//   - A WRITE TARGET IS NEVER PROPOSED. A dependency declared "write": true is
//     a row this folder writes, and the organization-wide exact-name match for
//     it in a new stack is the table the FIRST stack writes — production, for a
//     Dev copy. It is reported unresolved (write_target) and bound by hand,
//     per stack. A RUN TARGET ("run": true) is not proposed for the
//     same reason (run_target): the match is the workflow or agent
//     the first stack runs. An address and an output are never looked up
//     either; they are not bound at all.
//   - ONLY A NAME MATCH IS PROPOSED: the same name up to case, or — when none
//     is — up to case and separators (looseBindName). A prefix or substring hit
//     is a guess, and the cost of a wrong guess is not a failed command — it is
//     a workflow silently deployed against somebody else's table. Near misses
//     are reported and left alone.
//   - AMBIGUITY IS NEVER RESOLVED, --yes INCLUDED. `--yes` says "I trust the
//     unambiguous ones"; it cannot say which of three tables called `orders` was
//     meant, and picking one for a person who is not watching is the same wrong
//     deploy arrived at more quietly.
//   - IT WRITES ONLY WHAT IT PROPOSED, and only to `bind`. Nothing else in the
//     manifest is touched, and `ronja.lock.json` is not written at all: a bind is
//     config a person decided, not state a deploy discovered.
func newBindCmd() *cobra.Command {
	var yes bool
	var featureID string
	cmd := &cobra.Command{
		Use:   "bind",
		Short: "Point this folder's declared dependencies at this organization's rows",
		Long: `Point this folder's declared dependencies at this organization's rows.

A folder's ronja.json can declare DEPENDENCIES — the names its code uses for
rows it does not own:

  "dependencies": { "orders": {"kind": "table"} }

and each stack answers them with a "bind" map saying which of ITS organization's
rows each name means. That is what lets one folder deploy to two organizations:
the code says {{ ref('orders') }} everywhere, and the ids stay out of it.

This command fills in the missing half. For every declared name with no binding
on the selected stack it searches the organization for a resource of that kind
called that, and offers what it found:

  ronja bind --stack prod

A name matches when it is the same ignoring case, or — when nothing matches
that way — the same ignoring case and the difference between "_", "-", "."
and spaces, so qa_import_crm finds the secret "QA Import CRM". The plan says
when a match was of that second kind. If nothing is called that, or several
things are, it says so and leaves the name unbound — a near match is a guess,
and a wrong binding deploys your code against the wrong row without failing.
Codexes cannot be offered at all: the search does not cover them, so those are
set by hand. Nor is a write target (a dependency declared "write": true): which
table a stack may WRITE is decided by hand for each stack, because the row
called that name may be the one another stack writes.

Nothing is written until you confirm; --yes accepts the unambiguous ones without
asking, and still never picks between candidates. It exits non-zero while any
declared name is still unbound, so CI can run it as a check.

Promoting a folder to an organization it has never deployed to needs one more
thing — a stack to bind against — and --feature declares it in the same command:

  ronja bind --stack prod --feature col-...

The stack's url and organization come from your credential; --feature says which
feature its resources are created in, which is the one thing nothing can work out
for you. Without --feature, a stack the folder does not declare is refused rather
than half-written. --feature will not repoint a stack that already names a
different feature: that is an edit to ronja.json, not a flag.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			kind, err := folderKindHere()
			if err != nil {
				return err
			}
			// Refused HERE, before the folder is opened, because everything
			// below this line is about reconciling declared names and a module
			// has none to reconcile. Without it `bind` runs to completion on a
			// module folder and reports "0 names bound" — which is true, and
			// which a reader takes as "nothing was missing" rather than "this
			// command does not apply here".
			if kind.Name == wfdir.KindModule {
				return fmt.Errorf("this is a %s folder, and `ronja bind` has nothing to do in one: %s.\n  For what you CAN do here, see `ronja module --help`",
					kind.Label, moduleDependenciesInert)
			}
			// BEFORE openFolder, and that ordering is the whole point of the
			// check. openFolder runs adoptStack, which WRITES ronja.json when a
			// folder still in the legacy `instances[]` shape is named with
			// --stack — so a `bind --stack <existing> --feature <unreachable>`
			// that confirmed the feature later would rewrite the committed file
			// and only then refuse. The two things it needs are both known here:
			// the id came off the command line, and the credential is resolved.
			if err := confirmBindFeature(cmd.Context(), featureID, resolved); err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, kind)
			if err != nil {
				return err
			}
			result, err := runBind(cmd.Context(), f, yes, featureID)
			if err != nil {
				return err
			}
			if flagJSON {
				if err := emitJSON(result); err != nil {
					return err
				}
			} else {
				printBindOutcome(result)
			}
			return result.exitError()
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false,
		"write the unambiguous bindings without a prompt (required when there is no terminal)")
	cmd.Flags().StringVar(&featureID, "feature", "",
		"declare the --stack this names, creating new resources in this feature (only for a stack the folder does not have yet)")
	addStackFlag(cmd)
	return cmd
}

// folderKindHere works out which kind of folder this is, so a kind-agnostic
// command can open one.
//
// Every other folder command knows its kind from the verb it was typed under —
// `wf push` is a workflow folder or it is an error — and LoadManifest's
// wrong-kind refusal is what keeps `wf push` from making a workflow out of a
// data app's TSX. `bind` has no such verb, and it does not need the protection:
// it reads `dependencies` and writes `bind`, both of which every kind carries,
// and it touches no source file at all.
//
// A manifest this cannot read a kind out of is handed to LoadManifest anyway,
// under the workflow kind, because LoadManifest's refusals for a missing kind, a
// truncated file and an old-style "instances" object are the good errors for
// those cases and there is nothing to add to them here.
func folderKindHere() (wfdir.Kind, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return wfdir.WorkflowKind, fmt.Errorf("locate working directory: %w", err)
	}
	root, err := wfdir.FindRoot(cwd)
	if err != nil {
		if errors.Is(err, wfdir.ErrNoManifest) {
			// The list is the commands that CREATE a folder, which is not the
			// same set as the kinds this build can READ and is deliberately not
			// derived from wfdir's registry: a kind whose command group does not
			// exist yet has no business in advice about how to make one.
			//
			// `ronja module` is absent for a second reason again: it creates a
			// folder, but not one `bind` acts on — see moduleDependenciesInert.
			return wfdir.WorkflowKind, fmt.Errorf("no %s here or in any parent directory — run `ronja bind` inside a folder created by `ronja wf`, `ronja app`, `ronja pipeline` or `ronja automation`",
				wfdir.ManifestName)
		}
		return wfdir.WorkflowKind, err
	}
	kind, _, err := folderKindAt(root)
	return kind, err
}

// folderKindAt is folderKindHere for a root the caller already found, which is
// what `ronja sync` has after walking DOWN for manifests rather than up.
//
// Split so the probe itself exists once: it is the only kind-agnostic reader of
// a manifest in the CLI, and a second copy is exactly how the two would come to
// disagree about which folder is a pipeline.
//
// `known` reports whether the file DECLARED a kind this build understands, as
// opposed to declaring nothing, or declaring something it has never heard of —
// a `ronja db` folder hand-committed at a repository root, say. The kind
// returned in that case is still WorkflowKind, because `bind` wants
// LoadManifest's refusals for a missing kind and a truncated file and there is
// nothing to add to them; a caller that reports the kind to a reader wants the
// flag instead, so it does not print "workflow" about a folder that said
// otherwise.
//
// ⚠️ The set is wfdir's REGISTRY, not a switch here. It was a switch, over three
// kinds, and the fourth was added without it: an automation folder came back
// `known=false`, which is how `ronja sync status` came to exit 2 for a whole
// tree over a folder it understands perfectly. A registry lookup cannot be left
// behind that way — see wfdir.KindByName.
//
// ⚠️ `known` means THE BUILD UNDERSTANDS THIS KIND, never "this command can act
// on it". A module folder is known and `bind` does not act on one: it refuses
// it because a module declares no dependencies (see moduleDependenciesInert).
// Answering `false` for module would have been the cheap way to get that
// refusal, and it would have made it lie — and it would have told `ronja sync`,
// which covers module folders like every other kind, that this build does not
// understand a kind it has a whole command for.
func folderKindAt(root string) (kind wfdir.Kind, known bool, err error) {
	raw, err := os.ReadFile(wfdir.ManifestPath(root))
	if err != nil {
		return wfdir.WorkflowKind, false, fmt.Errorf("read %s: %w", wfdir.ManifestPath(root), err)
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	// The error is deliberately dropped: a file that will not parse is one
	// LoadManifest describes far better than a probe can, and it gets to.
	_ = json.Unmarshal(raw, &probe)
	if kind, ok := wfdir.KindByName(probe.Kind); ok {
		return kind, true, nil
	}
	return wfdir.WorkflowKind, false, nil
}

// folderKindLabels names every folder kind this build can READ, joined for a
// sentence: "automation, data app, pipeline and workflow".
//
// Derived from the registry rather than written out, because the sentence it
// serves is a claim about what a tree walk covers, and written out it said
// three while the walk covered four.
func folderKindLabels() string {
	kinds := wfdir.AllKinds()
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, kind.Label)
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// moduleDependenciesInert is the ONE wording of why the alias layer does nothing
// in a module folder, shared by `bind` (which refuses one) and `module status`
// (which flags a block already committed to one).
//
// The alias layer resolves a declared name INTO A MARKER, and module files carry
// no markers at all — that is the load-bearing rule of the whole primitive, the
// one `module validate` and `module push` enforce (see moduleMarkerProblem). So
// a module folder's `dependencies` block has nothing to fill: it is not merely
// unbound, it is unfillable, and every message about it has to say which of the
// two it means or the reader goes looking for the binding they are missing.
const moduleDependenciesInert = "modules are marker-free, so a module folder's `dependencies` block resolves nothing — there is no marker in a module file for an alias to fill. Declare the dependency in the CONSUMER workflow, which does carry markers, and pass the resolved value into this module's function as an argument"

// moduleInertDependenciesNote is the note `ronja sync check` and `sync apply`
// carry for a module folder that commits a `dependencies` block, or "" when it
// declares none. A note, never a refusal: the block stops nothing.
func moduleInertDependenciesNote(m *wfdir.Manifest) string {
	names := m.AliasNames()
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("%s declares the dependency name(s) %s, which will never resolve here — %s",
		wfdir.ManifestName, strings.Join(names, ", "), moduleDependenciesInert)
}

// Why an alias could not be proposed. These strings are the `reason` field of
// the --json output, so they are a contract with whatever reads it: each one
// says something a reader would act on differently.
const (
	// bindReasonNoMatch — nothing this credential can see is called that.
	bindReasonNoMatch = "no_match"
	// bindReasonAmbiguous — several things are, and choosing is a person's job.
	bindReasonAmbiguous = "ambiguous"
	// bindReasonNotSearchable — the kind has no search coverage, so "no match"
	// would be a lie. Today that is `codex` and nothing else.
	bindReasonNotSearchable = "not_searchable"
	// bindReasonTermTooShort — the alias is shorter than the search endpoint's
	// minimum term, so nothing was looked up at all.
	bindReasonTermTooShort = "term_too_short"
	// bindReasonUnknownKind — a declared kind this build has no search kind for.
	//
	// REACHABLE, and by the case it reads worst for: a folder written by a newer
	// CLI, declaring a kind that build resolves and this one has never heard of.
	// Load refuses only an EMPTY kind (Manifest.checkDependencies); the
	// unknown-kind refusal is CheckDeclarations, which lives at the acceptance
	// points a push and a validate call and which `bind` deliberately does not —
	// `bind` is the command you run to REPAIR a folder, so it opens one it cannot
	// fully act on rather than refusing to look at it. It is also what makes
	// ADDING a dependency kind and forgetting searchKindOf a named refusal rather
	// than a silent "not searchable".
	bindReasonUnknownKind = "unknown_kind"
	// bindReasonWriteTarget — a dependency declared "write": true. Never looked
	// up: a row this folder WRITES is chosen by hand for each stack, because
	// the exact-name match is the table another stack writes.
	bindReasonWriteTarget = "write_target"
	// bindReasonRunTarget — a dependency declared "run": true. Never looked up,
	// for the write target's reason: a run edge out of the stack's
	// feature runs with the target's own authority, and the exact-name match
	// is the row another stack runs.
	bindReasonRunTarget = "run_target"
)

// searchKindOf maps a DEPENDENCY kind onto the `kind` a search hit carries.
//
// The two vocabularies coincide word for word today, and this exists so nothing
// relies on that. They are separate contracts with separate owners: a dependency
// kind is part of the ronja.json FILE FORMAT (renaming one breaks every folder
// in the field), and a hit kind is the search endpoint's wire vocabulary. An
// explicit table also makes the ones that have NO counterpart legible — `codex`
// and `module` are absent because the endpoint fans out to neither, so no query
// can ever return one.
//
// ok is false for a kind with no search coverage; the caller has to say which of
// the two absences it is.
func searchKindOf(dependencyKind string) (kind string, ok bool) {
	switch dependencyKind {
	case markers.KindTable:
		return api.SearchKindTable, true
	case markers.KindSecret:
		return api.SearchKindSecret, true
	case markers.KindAgent:
		return api.SearchKindAgent, true
	case markers.KindMailbox:
		return api.SearchKindMailbox, true
	case markers.KindWorkflow:
		return api.SearchKindWorkflow, true
	case markers.KindNote:
		return api.SearchKindNote, true
	case markers.KindDataApp:
		return api.SearchKindDataApp, true
	case markers.KindMCPServer:
		return api.SearchKindMCPServer, true
	case markers.KindFeature:
		return api.SearchKindFeature, true
	default:
		return "", false
	}
}

// bindProposal is one alias this command is offering to bind, and to what.
type bindProposal struct {
	Alias string `json:"alias"`
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	// Title is the row's own name. It equals the alias up to case — or, when
	// Loose, up to case and separators — which is exactly why it is reported:
	// seeing the two side by side is how a reader checks that the match is the
	// one they meant.
	Title string `json:"title"`
	// Loose is true when no row is called the alias exactly and this one is
	// called it ignoring case and separators ("QA Import CRM" for
	// `qa_import_crm`). Said in the plan, so a reader approving it sees that the
	// two spellings differ.
	Loose bool `json:"loose,omitempty"`
}

// bindUnresolved is one alias this command will not bind, and why.
type bindUnresolved struct {
	Alias  string `json:"alias"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	// Detail is the sentence a person reads. It is in the JSON too, because the
	// reason alone does not say WHICH rows collided.
	Detail string `json:"detail"`
	// Candidates are the ids that matched, for bindReasonAmbiguous only.
	// They are the whole answer for a reader picking one by hand.
	Candidates []string `json:"candidates,omitempty"`
}

// bindResult is one run of the command.
type bindResult struct {
	// Stack is the stack acted on, empty for a folder on the older shape whose
	// binding lives in the unnamed "instances" entry.
	Stack string `json:"stack"`
	// Manifest is the file that was — or would have been — written.
	Manifest string `json:"manifest"`
	// AlreadyBound are the declared aliases this stack answers already. Reported
	// rather than silent, because "bind did nothing" and "there was nothing to
	// do" are different states and only one of them is fine.
	AlreadyBound []string `json:"alreadyBound"`
	// Proposed are the matches found. Written iff Written is true — a declined
	// prompt leaves them here and writes nothing.
	Proposed []bindProposal `json:"proposed"`
	// Unresolved are the aliases nothing could be proposed for.
	Unresolved []bindUnresolved `json:"unresolved"`
	// DeclaresStack is what --feature is about to add to the manifest besides
	// bindings, empty when the stack was already there. Reported because
	// declaring an environment is a bigger change than the bindings that
	// motivated it, and one a reviewer should see named.
	DeclaresStack string `json:"declaresStack,omitempty"`
	// Written reports whether the manifest was rewritten.
	Written bool `json:"written"`
}

// stillUnbound lists every declared alias that has no binding after this run, in
// the order every message names them.
func (r *bindResult) stillUnbound() []string {
	out := make([]string, 0, len(r.Unresolved)+len(r.Proposed))
	for _, u := range r.Unresolved {
		out = append(out, u.Alias)
	}
	if !r.Written {
		for _, p := range r.Proposed {
			out = append(out, p.Alias)
		}
	}
	sort.Strings(out)
	return out
}

// exitError is the non-zero exit an unbound dependency earns, so this command
// can be a CI check and not only an authoring aid.
//
// It reports the STATE the folder is in rather than what some later command will
// do about it. What a push does with an unbound alias is the push's business,
// and a message here that guessed at it would be wrong the moment that changed.
func (r *bindResult) exitError() error {
	left := r.stillUnbound()
	if len(left) == 0 {
		return nil
	}
	return fmt.Errorf("%s still unbound %s (%s) — set each one under \"bind\" in %s, or run `ronja bind` again once the rows exist there",
		dependencyCount(len(left)), r.where(), strings.Join(left, ", "), r.Manifest)
}

// where names the entry being written, the way wfdir's own refusals name it.
func (r *bindResult) where() string {
	if r.Stack == "" {
		return `in the unnamed "instances" entry`
	}
	return fmt.Sprintf("in stack %q", r.Stack)
}

// dependencyCount renders a count of declared names with its verb. `plural` is a
// suffix rule and "dependency" is not a suffix it can do, so this noun gets its
// own two lines rather than teaching that helper about English.
func dependencyCount(n int) string {
	if n == 1 {
		return "1 dependency is"
	}
	return fmt.Sprintf("%d dependencies are", n)
}

// bindCount renders "1 binding" / "3 bindings" — the count plural leaves to its
// caller.
func bindCount(n int, singular string) string {
	return fmt.Sprintf("%d %s", n, plural(n, singular))
}

// runBind resolves every unbound declaration and, with the caller's agreement,
// records the answers.
//
// Nothing is written until every alias has been resolved, so a search that fails
// half way through leaves the folder exactly as it was rather than half bound.
// requireStackForFeature is the one refusal --feature earns without looking at
// anything: a feature says where a STACK's resources are created, so naming no
// stack names nothing.
//
// Its own function because it is now asked twice — once before the folder is
// opened, so the check that follows costs no request on a command that was never
// going to run, and once in bindTargetOf, which is the acceptance point and must
// not depend on a caller having asked first.
func requireStackForFeature(featureID string) error {
	if featureID != "" && flagStack == "" {
		return fmt.Errorf("--feature says which feature a stack's resources are created in, and no stack was named — pass --stack <name> with it")
	}
	return nil
}

// confirmBindFeature is the pre-open half of `bind --feature`: refuse an
// unreachable feature while the folder on disk is still untouched.
//
// Declaring a stack with --feature is the documented way to promote a folder to
// a SECOND organization, which makes it the other door onto the silent bind
// `init` just closed — the same id written into the same committed file for an
// organization nobody has checked it against.
//
// The organization is resolved first because the refusal NAMES it, and a
// $RONJA_TOKEN credential does not know its own by construction. It is not an
// extra round trip: resolveBinding pays for it moments later on every folder
// that names anything on this instance, and runBind paid for it on the ones that
// do not.
func confirmBindFeature(ctx context.Context, featureID string, resolved *config.Resolved) error {
	if featureID == "" {
		return nil
	}
	if err := requireStackForFeature(featureID); err != nil {
		return err
	}
	if err := ensureTenant(ctx, resolved); err != nil {
		return err
	}
	return confirmFeatureIn(ctx, featureID, resolved)
}

func runBind(ctx context.Context, f *folder, yes bool, featureID string) (*bindResult, error) {
	// A stack is one organization on one instance, and wfdir.checkStacks refuses
	// one that names none — so DECLARING a stack needs an organization resolved,
	// and this is exactly the case resolveBinding skips: it asks only when the
	// folder already names something on this instance, and the whole point of
	// --feature is that this one does not.
	//
	// It matters most on the credential the promotion loop actually runs under.
	// config.Resolve clears the organization for a $RONJA_TOKEN credential by
	// construction, so without this a CI job promoting a folder to a new
	// organization would be refused for want of an answer one request away. Same
	// call bindNew makes, for the same reason.
	if featureID != "" && !f.Key.Known() {
		if err := ensureTenant(ctx, f.Resolved); err != nil {
			return nil, err
		}
		// Re-matched, not just re-keyed: knowing the organization can change which
		// entry this folder resolves to, and it is what turns `--stack prd` against
		// a folder that already calls this organization `prod` into the near-miss
		// refusal rather than a second set of resources.
		f.matchBinding()
		if f.BindingErr != nil {
			return nil, f.BindingErr
		}
	}

	sel := f.selection()
	result := &bindResult{
		Stack:        f.Stack,
		Manifest:     wfdir.ManifestPath(f.Root),
		AlreadyBound: []string{},
		Proposed:     []bindProposal{},
		Unresolved:   []bindUnresolved{},
	}

	// The cross-object bind rules, at the acceptance point Manifest.CheckBind
	// documents. Asked BEFORE anything is added, because every one of them is a
	// state that adding to would make worse: a bind naming a dependency nobody
	// declared is what a typo looks like, and appending the correctly-spelled one
	// beside it leaves the reader with two entries and no clue which is live.
	if err := f.Manifest.CheckBind(sel); err != nil {
		return nil, err
	}

	// WHERE the write will go, resolved before the first request. A folder with
	// nowhere to record an answer must be refused while the reader is still
	// reading their own command, not after they have approved a list.
	//
	// Ahead of the "declares nothing" exit below, unlike before --feature
	// existed: declaring a stack is now a thing this command can do on its own,
	// and `ronja bind --stack prod --feature col-…` in a folder with no
	// dependencies yet is a perfectly ordinary way to set an environment up.
	// Reporting "nothing to bind" and silently not declaring it would be a
	// command that ignored half of what it was told.
	target, err := bindTargetOf(f, featureID)
	if err != nil {
		return nil, err
	}
	result.DeclaresStack = target.declares

	aliases := f.Manifest.AliasNames()
	if len(aliases) == 0 && target.declares == "" {
		// Nothing to reconcile and nothing to declare.
		return result, nil
	}

	client := api.New(f.Resolved.URL, f.Resolved.Token)
	for _, alias := range aliases {
		dep := f.Manifest.Dependencies[alias]
		// An ADDRESS dependency resolves through the other folder's claim and
		// an OUTPUT through its name — neither is ever bound by search, and an
		// output must not be: the exact-name match for it would be the same
		// output in ANOTHER target, which is the one row it must never write.
		if dep.ByAddress() || dep.Output {
			continue
		}
		if f.Bind[alias] != "" {
			result.AlreadyBound = append(result.AlreadyBound, alias)
			continue
		}
		// A WRITE TARGET is never proposed. The search is
		// organization-wide, so the exact-name match for `ledger` in a new
		// stack is the very table the folder's first stack writes — production,
		// when the new stack is a Dev Space of the same organization — and
		// binding it there turns every run of the copy into a write to the
		// original. Which table a stack may write is a decision a person makes
		// per stack, by hand, and it is reported as such.
		if dep.Write {
			result.Unresolved = append(result.Unresolved, bindUnresolved{
				Alias: alias, Kind: dep.Kind, Reason: bindReasonWriteTarget,
				Detail: fmt.Sprintf("write target — bound by hand, per stack: this folder writes it, so a match by name could be the table another stack writes. Set it in %s under this stack's \"bind\"", wfdir.ManifestName),
			})
			continue
		}
		// A RUN EDGE is never proposed either, for the same reason:
		// which workflow or agent outside its feature a stack runs — with that
		// row's own authority, writes included — is decided per stack, by hand.
		if dep.Run {
			result.Unresolved = append(result.Unresolved, bindUnresolved{
				Alias: alias, Kind: dep.Kind, Reason: bindReasonRunTarget,
				Detail: fmt.Sprintf("run target — bound by hand, per stack: this folder runs it with its own authority, so a match by name could be the row another stack runs. Set it in %s under this stack's \"bind\"", wfdir.ManifestName),
			})
			continue
		}
		proposal, unresolved, err := proposeBind(ctx, client, alias, dep.Kind)
		if err != nil {
			return nil, err
		}
		if proposal != nil {
			result.Proposed = append(result.Proposed, *proposal)
			continue
		}
		result.Unresolved = append(result.Unresolved, *unresolved)
	}

	if len(result.Proposed) == 0 && target.declares == "" {
		return result, nil
	}
	if !flagJSON {
		printBindPlan(result, f.Manifest.Dependencies)
	}
	// ONE confirmation covering everything about to be written, the declaration
	// included. A prompt that mentioned only the bindings would be a prompt that
	// understated what it was asking for.
	change := bindCount(len(result.Proposed), "binding")
	if target.declares != "" {
		change = target.declares
		if len(result.Proposed) > 0 {
			change += " and " + bindCount(len(result.Proposed), "binding")
		}
	}
	ok, err := confirm(
		fmt.Sprintf("Record %s %s in %s?", change, result.where(), wfdir.ManifestName),
		fmt.Sprintf("writes %s into %s", change, wfdir.ManifestName),
		yes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return result, nil
	}
	if err := target.apply(result.Proposed); err != nil {
		return nil, err
	}
	// SaveFolder rather than SaveManifest, per its own contract — the two
	// committed files are written together. The lock is unchanged here (a bind is
	// config, and no id was created), so this is a manifest write with a no-op in
	// front of it.
	if err := f.saveFolder(); err != nil {
		return nil, fmt.Errorf("write %s: %w", wfdir.ManifestPath(f.Root), err)
	}
	result.Written = true
	return result, nil
}

// proposeBind answers one alias: the row to bind it to, or why there is none.
// Exactly one of the two returns is non-nil when err is nil.
func proposeBind(ctx context.Context, client *api.Client, alias, dependencyKind string) (*bindProposal, *bindUnresolved, error) {
	refuse := func(reason, detail string, candidates []string) (*bindProposal, *bindUnresolved, error) {
		return nil, &bindUnresolved{
			Alias: alias, Kind: dependencyKind, Reason: reason, Detail: detail, Candidates: candidates,
		}, nil
	}

	hitKind, searchable := searchKindOf(dependencyKind)
	switch {
	case dependencyKind == markers.KindCodex:
		// Said as what it IS, not as "no match". The search endpoint does not fan
		// out to codexes at all, so an empty result here would send the reader
		// hunting the web app for a codex that is sitting right there.
		return refuse(bindReasonNotSearchable,
			"search does not cover codexes — set this one by hand in "+wfdir.ManifestName, nil)
	case !searchable:
		return refuse(bindReasonUnknownKind,
			fmt.Sprintf("this ronja has no search kind for a %q dependency — set it by hand in %s, and upgrade the CLI (run: ronja update) if the kind is newer than this build", dependencyKind, wfdir.ManifestName), nil)
	case len(alias) < api.MinSearchTermLen:
		// The endpoint answers a too-short term with an empty result and no error,
		// which is indistinguishable on the wire from "nothing is called that".
		return refuse(bindReasonTermTooShort,
			fmt.Sprintf("the search needs at least %d characters and %q is shorter, so nothing was looked up — set this one by hand in %s, or rename it", api.MinSearchTermLen, alias, wfdir.ManifestName), nil)
	}

	hits, err := client.Search(ctx, alias, api.MaxSearchLimit)
	if err != nil {
		// Fatal to the whole run rather than recorded against this alias: an
		// instance that will not answer says nothing about any of the names, and
		// reporting "no exact match" for a question nobody got to ask is the one
		// output that would send a reader to change the wrong thing.
		return nil, nil, fmt.Errorf("search %s for %q: %w", client.BaseURL, alias, err)
	}
	exact, loose := matchBindHits(hits, hitKind, alias)
	// A FULL PAGE is the only signal there is that the result set was cut, and
	// without acting on it this command's own "AMBIGUITY IS NEVER RESOLVED, --yes
	// INCLUDED" rule is a promise it cannot keep. It asks for exactly the server's
	// hard cap and there is no paging, so a request that comes back full is not an
	// enumeration — it is a prefix of one. The tag leg is what makes that reachable
	// rather than theoretical: it stamps a matching TAG's score onto every row it
	// surfaces, so `orders` as a tag on forty rows fills the page with exact-scored
	// hits and the second table genuinely called `orders` never arrives. One
	// survivor would then be proposed, and under --yes written, as though it had
	// been the only candidate.
	//
	// So a full page never PROPOSES. It is reported as ambiguous, with the
	// candidate it did see named, because that is the state the folder is in: the
	// answer may be right and nothing here can show that it is.
	truncated := len(hits) >= api.MaxSearchLimit

	// At most TWO more requests, and only when no exact match turned up: the
	// search escapes `_` and matches the term as a substring, so `qa_import_crm`
	// never finds the secret called "QA Import CRM" — the name an imported
	// folder's alias is most often a spelling of.
	//
	//   - The SPACED form first ("qa import crm"): it finds that display name,
	//     and its page filling is truncation like the first one's.
	//   - Then the alias's LONGEST WORD (looseBindTerm), only to rule out a
	//     second loose match the spaced form cannot see — "qa import crm" is no
	//     substring of "qa-import-crm", "QA.Import.CRM" or "QA  Import CRM", and
	//     the one it missed would leave the other proposed as unambiguous. A
	//     loose match it finds counts like any other; its page filling does
	//     NOT block a proposal, because a common word ("orders") fills a page
	//     in any organization, and that says nothing about the spelling the
	//     spaced search saw whole.
	//
	// The hits are filtered here by looseBindName. Asked even when the first
	// search found a loose match, because a second loose match the first search
	// could not see is exactly the ambiguity that must not be resolved silently.
	if term, ok := looseBindTerm(alias); len(exact) == 0 && ok {
		spaced := looseBindName(alias)
		for _, q := range slices.Compact([]string{spaced, term}) {
			more, err := client.Search(ctx, q, api.MaxSearchLimit)
			if err != nil {
				return nil, nil, fmt.Errorf("search %s for %q: %w", client.BaseURL, q, err)
			}
			_, moreLoose := matchBindHits(more, hitKind, alias)
			loose = appendNewHits(loose, moreLoose)
			if q == spaced {
				truncated = truncated || len(more) >= api.MaxSearchLimit
			}
		}
	}

	// An exact match wins outright, so a folder that bound before loose matching
	// existed binds to the same row now. Only with none is a loose one offered.
	matched, how := exact, "called"
	if len(exact) == 0 {
		matched, how = loose, "called, ignoring case and separators,"
	}

	switch {
	case len(matched) == 1 && truncated:
		return refuse(bindReasonAmbiguous,
			fmt.Sprintf("the search filled its maximum of %d results, so this is not a complete list of what is %s %q — %s looks right, but nothing here can rule out a second one. Set it by hand in %s if it is the one you mean",
				api.MaxSearchLimit, how, alias, matched[0].ID, wfdir.ManifestName),
			[]string{matched[0].ID})
	case len(matched) == 1:
		return &bindProposal{Alias: alias, Kind: dependencyKind, ID: matched[0].ID, Title: matched[0].Title, Loose: len(exact) == 0}, nil, nil
	case len(matched) == 0:
		detail := fmt.Sprintf("no %s here is called %q, even ignoring case and separators", dependencyKind, alias)
		if truncated {
			// Nothing is written either way, so this is not the invariant above —
			// it is the reader being told not to trust a flat "nothing is called
			// that" when the search plainly ran out of room.
			detail += fmt.Sprintf(" (the search filled its maximum of %d results, so it may not have reached one)", api.MaxSearchLimit)
		}
		switch dependencyKind {
		case markers.KindSecret:
			// Worth saying only for secrets: the search drops the whole secret leg
			// for a scoped token with no secrets:read grant, so this is the one
			// kind whose "nothing found" can be about the CREDENTIAL rather than
			// about the organization.
			detail += " (a token scoped without secrets:read sees no secrets at all here)"
		case markers.KindMailbox:
			// The same family of hint, for the same reason and a different gate:
			// the endpoint's mailbox leg answers tenant ADMINS only (see
			// api/v2/search's own note), so an ordinary member's empty result is
			// about who they are rather than about what exists. Left as "no
			// match" it sends them looking for a mailbox that is sitting right
			// there — the failure the codex message exists to prevent one kind
			// over.
			detail += " (mailbox search answers tenant admins only, so a non-admin credential sees none)"
		}
		return refuse(bindReasonNoMatch, detail, nil)
	default:
		ids := make([]string, 0, len(matched))
		for _, hit := range matched {
			ids = append(ids, hit.ID)
		}
		sort.Strings(ids)
		return refuse(bindReasonAmbiguous,
			fmt.Sprintf("%d %ss here are %s %q — pick the one you mean and set it by hand", len(matched), dependencyKind, how, alias),
			ids)
	}
}

// matchBindHits splits a search result into the hits of the dependency's kind
// that are called alias EXACTLY (up to case) and those called it LOOSELY — the
// same name once case and separators are ignored (see looseBindName).
//
// The two legs check different things, on purpose:
//
//   - An exact hit needs the server's exact score AND the title. ⚠️ DO NOT
//     DELETE THE TITLE CHECK AS REDUNDANT WITH THE SCORE. It is not a second
//     opinion about the score; it is the only thing checking the name at all.
//     backend/api/v2/search/tags.go merges a TAG leg into the same result set
//     and stamps each hit's Score from its matching TAG's score — "a
//     tag-surfaced hit is indistinguishable from a name-match hit apart from
//     Score provenance", in its own words. So a table merely TAGGED `orders`
//     arrives here scoring 3 whatever it is actually called, and binding
//     through one resolves `{{ ref('orders') }}` to a table called something
//     else — under --yes, with no prompt, reported as a success. Folded,
//     because the server's own exact-match test is ILIKE: case sensitivity here
//     would silently reject the `Orders` that scored 3.
//   - A loose hit is judged on its title alone, because the score is the
//     server's verdict on whichever TERM was sent, and the term that finds
//     "QA Import CRM" is not the alias. A title spelled exactly like the alias
//     is never a loose hit: if the server did not score it exact, the two
//     disagree, and a disagreement is not something to bind through.
func matchBindHits(hits []api.SearchHit, hitKind, alias string) (exact, loose []api.SearchHit) {
	want := looseBindName(alias)
	for _, hit := range hits {
		if hit.Kind != hitKind {
			continue
		}
		switch {
		case strings.EqualFold(hit.Title, alias):
			if hit.Score == api.SearchScoreExact {
				exact = append(exact, hit)
			}
		case looseBindName(hit.Title) == want:
			loose = append(loose, hit)
		}
	}
	return exact, loose
}

// looseBindName is the one definition of "the same name, ignoring case and
// separators": lower-cased, every run of `_`, `-`, `.` and whitespace turned
// into a single space, and none at either edge. So `qa_import_crm`,
// "QA Import CRM", `qa-import-crm` and " QA  import.CRM " are one name, while
// `qaimportcrm` is another — a separator that is missing in the middle is a
// different word boundary, not a spelling of the same one.
func looseBindName(s string) string {
	return strings.ToLower(strings.Join(strings.FieldsFunc(s, isBindNameSeparator), " "))
}

// looseBindTerm is the last search's term: the alias's longest word (the
// first, of equally long ones). The server matches a term as a substring, and
// every spelling looseBindName folds to the alias's contains each of its words,
// so one request reaches all of them while its page lasts. ok is false for an
// alias that is its own loose name — the first search already asked for it
// whole — or whose longest word is too short to search for.
func looseBindTerm(alias string) (term string, ok bool) {
	if looseBindName(alias) == strings.ToLower(alias) {
		return "", false
	}
	for _, w := range strings.FieldsFunc(alias, isBindNameSeparator) {
		if len(w) > len(term) {
			term = w
		}
	}
	return strings.ToLower(term), len(term) >= api.MinSearchTermLen
}

func isBindNameSeparator(r rune) bool {
	return r == '_' || r == '-' || r == '.' || unicode.IsSpace(r)
}

// appendNewHits adds the hits of more that are not already in hits — the same
// row found by both searches is one candidate, not two.
func appendNewHits(hits, more []api.SearchHit) []api.SearchHit {
	seen := make(map[string]bool, len(hits))
	for _, hit := range hits {
		seen[hit.ID] = true
	}
	for _, hit := range more {
		if !seen[hit.ID] {
			seen[hit.ID] = true
			hits = append(hits, hit)
		}
	}
	return hits
}

// bindTarget is the entry a run writes its answers into.
//
// Resolved up front and carried as a closure so the two shapes — a named stack
// and the older unnamed "instances" entry — are told apart once, before any
// request, rather than at the moment of writing where a refusal costs the reader
// the approval they just gave.
type bindTarget struct {
	// declares is the structural change this write makes to the manifest beyond
	// the bind map — "declare stack \"prod\" (feature col-…)" — or empty when the
	// stack is already there. It is a SENTENCE rather than a bool because it is
	// what the prompt and the report say out loud, and declaring an environment
	// is not something to slip in under a count of bindings.
	declares string
	apply    func(accepted []bindProposal) error
}

func bindTargetOf(f *folder, featureID string) (bindTarget, error) {
	m := f.Manifest
	// Gated on the FLAG, not on the resolved stack, and the difference is the
	// whole rule. A folder that matches a stack implicitly resolves f.Stack
	// perfectly well, so gating on that would let `ronja bind --feature col-…`
	// with no --stack reach into whichever environment this credential happened
	// to match. Declaring or completing an environment is something a person
	// names; it is not something a credential selects.
	if err := requireStackForFeature(featureID); err != nil {
		return bindTarget{}, err
	}
	if f.Stack != "" {
		stack, declared := m.Stacks[f.Stack]
		switch {
		case declared && featureID != "" && stack.FeatureID != "" && stack.FeatureID != featureID:
			// Repointing an environment at a different feature changes where every
			// future resource is created, which is a decision to make in the file
			// under review rather than a flag on a command about aliases.
			//
			// Note what is NOT refused: the SAME feature the stack already names.
			// That is a no-op, and refusing it would mean a CI job could not run the
			// same `bind --stack prod --feature col-…` line twice.
			return bindTarget{}, fmt.Errorf("stack %q already creates its resources in %s, and --feature names %s. Repointing an environment is an edit to %s, not a flag — drop --feature to bind against the stack as it stands",
				f.Stack, stack.FeatureID, featureID, wfdir.ManifestName)
		case !declared && featureID == "":
			// Reached by `--stack prod` on a folder that has no prod, which is the
			// promotion case: a second organization, nothing pushed to it yet. It is
			// REFUSED rather than answered by writing the stack, because a stack
			// declared with no feature is a half-answer in a committed file that
			// reads as a decision somebody made — the hazard adoptStack declines for
			// the same reason. --feature is how a person supplies the missing half.
			return bindTarget{}, fmt.Errorf("%s declares no stack %q, so there is nothing to bind against yet. Declare it here with `--feature <id>` — the feature its resources are created in, which is the one thing this cannot work out for you — or add \"url\", \"tenantID\" and \"featureID\" to %s by hand",
				wfdir.ManifestPath(f.Root), f.Stack, wfdir.ManifestName)
		}

		declares := ""
		if featureID != "" && stack.FeatureID != featureID {
			if declared {
				// Declared with no feature at all: a stack somebody added by hand and
				// left half-finished, or one this command created before. Filling the
				// gap is the same act as declaring it, so it is reported as a change.
				declares = fmt.Sprintf("stack %q's feature (%s)", f.Stack, featureID)
			} else {
				declares = fmt.Sprintf("stack %q (feature %s)", f.Stack, featureID)
			}
		}
		if declares != "" && !f.Key.Known() {
			// wfdir.checkStacks refuses a stack with no "tenantID", so writing one
			// here would produce a manifest the very next command cannot open — the
			// one thing this package must never do. runBind resolves the
			// organization before it gets here, which makes this a guard against a
			// future path that forgets rather than a case anybody hits.
			return bindTarget{}, fmt.Errorf("cannot declare stack %q for %s: this credential's organization is not known, and a stack is one organization on one instance. Sign in with `ronja login`, or use a profile that records its organization",
				f.Stack, f.Key.URL)
		}
		return bindTarget{declares: declares, apply: func(accepted []bindProposal) error {
			stack := m.Stacks[f.Stack]
			if declares != "" {
				// Normalized exactly as Manifest.Record normalizes it, so a stack this
				// command declares and one a push declares are the same bytes — the
				// URL is matched through config.NormalizeURL, and two spellings of one
				// instance in a committed file is how a folder reads as unbound
				// against the instance it is plainly bound to.
				url := f.Key.URL
				if norm, err := config.NormalizeURL(url); err == nil {
					url = norm
				}
				stack.URL, stack.TenantID, stack.FeatureID = url, f.Key.TenantID, featureID
			}
			stack.Bind = mergedBind(stack.Bind, accepted)
			if m.Stacks == nil {
				m.Stacks = map[string]wfdir.Stack{}
			}
			m.Stacks[f.Stack] = stack
			return nil
		}}, nil
	}
	if !f.Bound {
		return bindTarget{}, fmt.Errorf("%s names nothing on %s, so there is no entry to record a binding in. Push the folder there first, or name a stack with --stack <name>",
			wfdir.ManifestPath(f.Root), describeTarget(f.Resolved))
	}
	index, err := wfdir.Find(m.Instances, wfdir.Instance.Key, f.Key)
	if err != nil {
		return bindTarget{}, err
	}
	if index < 0 {
		// Bound with no stack and no matching entry is not a state Manifest.Select
		// can produce, so this is a guard against a future one rather than a case
		// anybody hits — and a wrong write here would put a binding in a file where
		// nothing looks for it.
		return bindTarget{}, fmt.Errorf("%s names %s but no entry for it could be found to record a binding in",
			wfdir.ManifestPath(f.Root), describeTarget(f.Resolved))
	}
	return bindTarget{apply: func(accepted []bindProposal) error {
		m.Instances[index].Bind = mergedBind(m.Instances[index].Bind, accepted)
		return nil
	}}, nil
}

// mergedBind returns a NEW map holding what was bound before plus what was just
// accepted.
//
// A copy rather than a write through the existing map, for the reason
// Binding.WithTable is one: the map is a reference shared with the Selection this
// run has been reading, and mutating it in place would "record" a binding before
// the save that makes it real — and keep it after a save that failed.
func mergedBind(existing map[string]string, accepted []bindProposal) map[string]string {
	out := make(map[string]string, len(existing)+len(accepted))
	for alias, id := range existing {
		out[alias] = id
	}
	for _, p := range accepted {
		out[p.Alias] = p.ID
	}
	return out
}

// printBindPlan shows what is about to be written, before the prompt that
// approves it. Everything this run learned, not only the proposals: a reader
// deciding whether to accept three bindings wants to see the two it could not
// make in the same list.
func printBindPlan(r *bindResult, deps map[string]wfdir.Dependency) {
	out := os.Stdout
	if r.DeclaresStack != "" {
		fmt.Fprintf(out, "  This declares %s in %s.\n\n", r.DeclaresStack, wfdir.ManifestName)
	}
	fmt.Fprintf(out, "  Dependencies of this folder %s:\n\n", r.where())
	width := 0
	for _, alias := range r.declaredAliases() {
		if len(alias) > width {
			width = len(alias)
		}
	}
	for _, p := range r.Proposed {
		how := ""
		if p.Loose {
			how = "  (matched ignoring case and separators)"
		}
		fmt.Fprintf(out, "    %-*s  %-8s  → %s  %q%s\n", width, p.Alias, p.Kind, p.ID, p.Title, how)
	}
	for _, u := range r.Unresolved {
		fmt.Fprintf(out, "    %-*s  %-8s  %s\n", width, u.Alias, u.Kind, u.Detail)
	}
	for _, alias := range r.AlreadyBound {
		// The kind comes from the manifest rather than from the result, which
		// carries it only for the two groups where something had to be decided.
		// An empty column here reads as a hole in the table, and the reader would
		// have to guess whether it means something.
		fmt.Fprintf(out, "    %-*s  %-8s  already bound\n", width, alias, deps[alias].Kind)
	}
	fmt.Fprintln(out)
}

// declaredAliases is every name this run looked at, whichever group it landed
// in — the column width has to fit all three or the table stops lining up.
func (r *bindResult) declaredAliases() []string {
	out := make([]string, 0, len(r.Proposed)+len(r.Unresolved)+len(r.AlreadyBound))
	for _, p := range r.Proposed {
		out = append(out, p.Alias)
	}
	for _, u := range r.Unresolved {
		out = append(out, u.Alias)
	}
	return append(out, r.AlreadyBound...)
}

// printBindOutcome says what happened, after the fact. The plan above has
// already listed the detail when there was anything to approve.
func printBindOutcome(r *bindResult) {
	out := os.Stdout
	switch {
	case r.Written:
		// Two sentences rather than one, because a run can do either or both, and
		// "recorded 0 bindings" is what one sentence says when --feature declared a
		// stack in a folder whose names it could not resolve.
		switch {
		case r.DeclaresStack != "" && len(r.Proposed) > 0:
			fmt.Fprintf(out, "  Declared %s in %s.\n", r.DeclaresStack, r.Manifest)
			fmt.Fprintf(out, "  Recorded %s there.\n", bindCount(len(r.Proposed), "binding"))
		case r.DeclaresStack != "":
			fmt.Fprintf(out, "  Declared %s in %s.\n", r.DeclaresStack, r.Manifest)
		default:
			fmt.Fprintf(out, "  Recorded %s %s in %s.\n", bindCount(len(r.Proposed), "binding"), r.where(), r.Manifest)
		}
		fmt.Fprintf(out, "  %s is committed and shared, so commit this with the code that uses the names.\n", wfdir.ManifestName)
	case len(r.Proposed) > 0 || r.DeclaresStack != "":
		fmt.Fprintf(out, "  Nothing was written.\n")
	case len(r.AlreadyBound) == 0 && len(r.Unresolved) == 0:
		fmt.Fprintf(out, "  This folder declares no dependencies, so there is nothing to bind.\n")
		fmt.Fprintf(out, "  Declare the names its code uses under \"dependencies\" in %s first.\n", wfdir.ManifestName)
	case len(r.Unresolved) == 0:
		fmt.Fprintf(out, "  Every dependency this folder declares is already bound %s (%s).\n",
			r.where(), strings.Join(r.AlreadyBound, ", "))
	default:
		// Nothing to propose and something unresolved: the plan never printed, so
		// the reasons are printed here instead.
		fmt.Fprintf(out, "  Nothing could be proposed %s:\n\n", r.where())
		for _, u := range r.Unresolved {
			fmt.Fprintf(out, "    %s  %s\n", u.Alias, u.Detail)
		}
		fmt.Fprintln(out)
	}
}
