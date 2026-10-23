package commands

import (
	"context"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// THE WRITE GATE: a deployment to a stack writes
// only inside that stack's own feature, except through a write-capable data
// source bound FOR THAT STACK.
//
// It exists for the slip scenario: `ronja import feature Sales` from
// Main, a `dev` stack added by hand, and `sync apply --stack dev
// --allow-create`. The imported workflow's `tools.replaceTable("table-…")` is
// an output literal no codec rewrites, so it still names Main's table — and
// without this gate the Dev deployment overwrites production on its first run,
// whatever the dev stack binds. The gate refuses it before anything is written.
//
// WHERE IT APPLIES: every stack of an ADOPTED folder (folder.adoptedHere — a
// folder id, a named stack with a feature, not unmanaged), on every path that
// puts code or grants in front of that stack: `sync apply` (at plan time — see
// planWriteGate — and inside the push for every write), the single-folder
// push, the publish, `wf test`, which runs the draft, `app test`, which
// renders a row with its grants, and `ronja <kind> pull`,
// which refuses before writing a file to take in a live row the stack could not
// deploy. The push gates BEFORE its "live already holds this folder" shortcut,
// so a folder that writes outside is refused whether or not it changed. A
// classic folder (no folder id) is checked exactly as before.
//
// WHAT IT CHECKS, per kind:
//
//   - a WORKFLOW: the server's own scan of the files about to be sent — POST
//     /workflow/validate's writeTargets, computed from the posted files after
//     the stack's codec resolved them (one scanner, not two). The WHOLE folder
//     is always posted, never a changed subset, because the row's output
//     bindings are derived from all of its files.
//   - a DATA APP: its `access.allowedCodexIDs` — the app token's codex grant
//     covers upsert and delete, so a codex there is written (the reference
//     inventory's read_write). Its table allowlist only reads.
//   - an AUTOMATION: the tables (`model`) in an agent action's reference set — a
//     headless run may write rows into any table it was given (read_write) —
//     and the FEATURES there, which the run expands to every table of that
//     feature and may write as well. `"@feature"`, the stack's own
//     feature, is inside by definition.
//   - a PIPELINE: its docs sidecars (`tables/<alias>.json`), each of which
//     writes its table's description and column prose — a table the folder
//     does not build, so nothing places it in the stack's feature. The tables
//     it builds are created in that feature by construction.
//
// THE RULE, per target:
//
//   - a table, codex or feature: in the stack's own feature (readable, and its
//     feature is the stack's; a feature is "in" itself), or the id a
//     write-capable data source ("write": true) of its kind is bound to IN THIS
//     STACK. An `output` dependency is NOT a write
//     source: bound in another stack's feature, it is refused like any table.
//     An unreadable one is outside — nothing can say where it lands.
//   - a table written by NAME (`{{ write('orders_daily') }}`): admitted; the
//     runtime creates or reuses it in the run's own feature.
//   - a folder write scope: a path every feature in the organization shares,
//     so it is never "inside" a feature. Admitted only when a folder data
//     source (`"kind": "folder", "write": true`) is bound IN THIS STACK to that
//     path — a deliberate, per-stack decision, and nothing a new
//     stack inherits.
//
// Anything else is refused `writes_outside_target`, naming the file and line
// (or the manifest field), the target and — when it is readable — its feature.
//
// RUN EDGES. Running a workflow or a saved agent is acting with
// that row's own authority, so a deployment that runs one in ANOTHER feature
// is as deliberate as one that writes there. Checked after the writes, on the
// same paths:
//
//   - a WORKFLOW's `{{ workflow() }}` and `{{ agent() }}` targets, as
//     validate RESOLVED them (its `resolved.workflowIDs` / `agentIDs` — the
//     server's own resolution of the posted files; the CLI only finds the line
//     a resolved id sits on, to name it) — and, failing closed, each one it
//     could NOT resolve (its unresolved_workflow / unresolved_agent findings,
//     and a leg over the binding cap), which is outside unless bound;
//   - a DATA APP's `access.allowedWorkflowIDs` / `allowedAgentIDs`;
//   - an AUTOMATION's `workflow` / `saved_agent` action and its `workflow`
//     references.
//
// A target passes when it is in the stack's own feature, when it is a row this
// repository manages in this stack (an address dependency resolved it), or
// when a dependency declared "run": true binds it IN THIS STACK. Anything else
// is refused `runs_outside_target`, naming the position, the target and its
// feature. What the bound row then DOES is its own business: once a stack has
// decided to run it, it runs with its own authority, writes included.

// syncReasonWritesOutsideTarget — a stack would write outside its own feature
// through something no write-capable data source of that stack binds. Machine
// contract, like every sync reason: add, never rename.
const syncReasonWritesOutsideTarget = "writes_outside_target"

// syncReasonNoWriteTargets — the instance answered validate with no
// writeTargets at all: it is older than this CLI, so nothing can say where an
// adopted folder's code writes, and the gate refuses rather than wave it
// through. Its own reason, not writes_outside_target, so a script branching on
// the reason does not read "your code writes another feature" for "the
// instance is too old". Machine contract: add, never rename.
const syncReasonNoWriteTargets = "no_write_targets"

// syncReasonRunsOutsideTarget — a stack would run a workflow or a saved agent
// outside its own feature that no "run": true dependency of that stack binds.
// Machine contract: add, never rename.
const syncReasonRunsOutsideTarget = "runs_outside_target"

// writeAuthority is what one stack has bound on purpose: the ids (and folder
// paths) its write-capable data sources resolve to, by kind — and the rows it
// runs on purpose, or manages.
type writeAuthority struct {
	featureID string
	stack     string
	// bound maps a kind to id → the alias that binds it (for a folder, the
	// normalized path → alias).
	bound map[string]map[string]string
	// run maps a kind (workflow, agent) to id → the "run": true alias that
	// binds it in this stack.
	run map[string]map[string]string
	// managed maps a kind to id → the ADDRESS dependency that resolved to it:
	// a row this repository manages in this stack, never outside it.
	managed map[string]map[string]string
	// managedFeature is the feature each managed row's claim is in, by alias
	// (addressEdge.FeatureID) — known without a request.
	managedFeature map[string]string
}

// writeAuthorityOf collects the stack's write binds: a dependency declared
// "write": true and bound in the selected stack. Address dependencies never
// write (the shape check refuses "from" beside "write"), and an output is the
// folder's own table, admitted by name or by feature — never by its bind. It
// collects the stack's run binds ("run": true, bound here) and what its address
// dependencies resolved to beside them — except a table another folder's
// workflow writes (addressEdge.Written): this folder only reads it, and the
// row may be bound there to another team's feature, so writing it (a docs
// sidecar) takes this folder's own write binding or the stack's own feature.
func (f *folder) writeAuthorityOf() writeAuthority {
	out := writeAuthority{featureID: f.Binding.FeatureID, stack: f.Stack, bound: map[string]map[string]string{},
		run: map[string]map[string]string{}, managed: map[string]map[string]string{}, managedFeature: map[string]string{}}
	written := map[string]bool{}
	for _, e := range f.addressEdges {
		out.managedFeature[e.Alias] = e.FeatureID
		written[e.Alias] = e.Written
	}
	add := func(into map[string]map[string]string, kind, id, alias string) {
		if into[kind] == nil {
			into[kind] = map[string]string{}
		}
		into[kind][id] = alias
	}
	for _, alias := range f.Manifest.AliasNames() {
		dep := f.Manifest.Dependencies[alias]
		if dep.ByAddress() {
			if id := f.addressBind[alias]; id != "" && !written[alias] {
				add(out.managed, dep.Kind, id, alias)
			}
			continue
		}
		id := f.Bind[alias]
		if id == "" {
			continue
		}
		if dep.Run {
			add(out.run, dep.Kind, id, alias)
		}
		if !dep.Write {
			continue
		}
		if markers.IsPathKind(dep.Kind) {
			norm, ok := wfdir.NormalizeFolderPath(id)
			if !ok || norm == "" {
				continue
			}
			id = norm
		}
		add(out.bound, dep.Kind, id, alias)
	}
	return out
}

// pendingAddress reports that id is the alias of an address dependency of
// `kind` that has not resolved only because the folder it names has no claim
// at the address in this stack yet — what `sync apply` defers to a row the same
// run creates (addressPending). Until then the codec leaves the alias spelling
// in place, so that is the id a plan-time gate sees.
func (f *folder) pendingAddress(kind, id string) bool {
	p, ok := f.addressPending[id]
	return ok && p.Kind == kind
}

// boundAs answers the alias a write source of `kind` binds `id` to here.
func (a writeAuthority) boundAs(kind, id string) (string, bool) {
	alias, ok := a.bound[kind][id]
	return alias, ok
}

// gateOutside is one write the gate refused: where it is written and what it
// names.
type gateOutside struct {
	Where   string
	Target  string
	Feature string
	Why     string
	// Verb is what the position does to the target: "writes" when empty, or
	// "runs".
	Verb string
}

func (o gateOutside) String() string {
	verb := o.Verb
	if verb == "" {
		verb = "writes"
	}
	return fmt.Sprintf("%s %s %s — %s", o.Where, verb, o.Target, o.Why)
}

// refuseWritesOutside turns the refused writes into the gate's one refusal.
// nil when there are none.
func refuseWritesOutside(f *folder, outside []gateOutside) error {
	if len(outside) == 0 {
		return nil
	}
	sort.SliceStable(outside, func(i, j int) bool { return outside[i].Where < outside[j].Where })
	lines := make([]string, 0, len(outside))
	docs := false
	for _, o := range outside {
		lines = append(lines, "    "+o.String())
		docs = docs || o.Verb == "documents"
	}
	sidecar := ""
	if docs {
		sidecar = fmt.Sprintf("\n  A docs file (%s/<alias>.json) writes its table's description and column prose: declare its alias the same way, or remove the file if another stack documents that table.", wfdir.TableDocsDirName)
	}
	return &managedRefusal{Reason: syncReasonWritesOutsideTarget, Detail: fmt.Sprintf(
		"%s deploys to feature %s, and this folder would write outside it — bind each target below as a write source for that stack, or write into its own feature:\n%s\n  To bind one, declare it under \"dependencies\" with \"write\": true and bind it in %s's \"bind\" to a row this stack may write. To write this feature instead, a `{{ write('name') }}` by name lands in the run's own feature.%s\n  A deployment writes only its own feature, except through a write-capable data source bound for that stack. Nothing was written",
		describeStackFor(f.Stack), orUnknown(f.Binding.FeatureID), strings.Join(lines, "\n"), describeStackFor(f.Stack), sidecar)}
}

// refuseRunsOutside turns the refused run edges into the gate's one refusal.
// nil when there are none.
func refuseRunsOutside(f *folder, outside []gateOutside) error {
	if len(outside) == 0 {
		return nil
	}
	sort.SliceStable(outside, func(i, j int) bool { return outside[i].Where < outside[j].Where })
	lines := make([]string, 0, len(outside))
	for _, o := range outside {
		lines = append(lines, "    "+o.String())
	}
	return &managedRefusal{Reason: syncReasonRunsOutsideTarget, Detail: fmt.Sprintf(
		"%s deploys to feature %s, and this folder would run outside it — bind each target below as a run edge for that stack, or point the folder at a row of its own feature:\n%s\n  To bind one, declare it under \"dependencies\" with \"run\": true and bind it in %s's \"bind\" to the row this stack may run.\n  A deployment runs only rows of its own feature or rows this repository manages in that stack, except through a run edge bound for that stack: running a workflow or an agent acts with its authority, writes included. Nothing was written",
		describeStackFor(f.Stack), orUnknown(f.Binding.FeatureID), strings.Join(lines, "\n"), describeStackFor(f.Stack))}
}

// runEdge is one place a folder RUNS a workflow or a saved agent: where it is
// written, the kind (markers.KindWorkflow / KindAgent) and the id it names.
type runEdge struct {
	Where string
	Kind  string
	ID    string
}

// gateRunEdges is the run rule over a folder's run edges.
func gateRunEdges(ctx context.Context, client *api.Client, f *folder, edges []runEdge) error {
	if !f.adoptedHere() || len(edges) == 0 {
		return nil
	}
	auth := f.writeAuthorityOf()
	type answer struct {
		feature, name string
		err           error
	}
	seen := map[string]answer{}
	var outside []gateOutside
	read := func(e runEdge) answer {
		key := e.Kind + "\x00" + e.ID
		got, asked := seen[key]
		if !asked {
			got.feature, got.name, got.err = runTargetOf(ctx, client, e.Kind, e.ID)
			seen[key] = got
		}
		return got
	}
	for _, e := range edges {
		if e.ID == "" {
			continue
		}
		if alias, ok := auth.managed[e.Kind][e.ID]; ok {
			// A row this repository manages in this stack: admitted — and
			// noted when its claim is in another feature, which is what a stack
			// block copied from another stack looks like (its folder's stack
			// still names that stack's feature). Known without a request.
			if feature := auth.managedFeature[alias]; feature != "" && feature != auth.featureID {
				f.noteManagedOutside(auth, e.Where, "runs", fmt.Sprintf("the %s %s", e.Kind, e.ID), alias, feature)
			}
			continue
		}
		if f.pendingAddress(e.Kind, e.ID) {
			// An address dependency still in its alias spelling, because the
			// row it names is created by this same run (a fresh stack): one this
			// repository will manage in this stack. The push re-runs the gate
			// once the address resolves, and an address that never does is
			// refused by the alias pre-flight, so this admits nothing the push
			// will not check against a real row.
			continue
		}
		_, bound := auth.run[e.Kind][e.ID]
		got := read(e)
		target := fmt.Sprintf("the %s %s", e.Kind, e.ID)
		if got.err == nil && got.name != "" {
			target = fmt.Sprintf("the %s %s (%s)", e.Kind, e.ID, got.name)
		}
		switch status := api.StatusOf(got.err); {
		case got.err == nil && got.feature != "" && got.feature == auth.featureID:
			continue
		case bound:
			// Deliberate: a run edge declared and bound for this stack.
			if got.err == nil {
				f.noteBoundOutside(auth, e.Where, "runs", target, auth.run[e.Kind][e.ID], e.ID, got.feature)
			}
			continue
		case got.err == nil:
			outside = append(outside, gateOutside{Verb: "runs", Where: e.Where, Target: target, Feature: got.feature,
				Why: describeFeatureOf(got.feature) + ", and no run source (\"run\": true) of this stack binds it"})
		case status == 400 || status == 403 || status == 404:
			outside = append(outside, gateOutside{Verb: "runs", Where: e.Where, Target: target,
				Why: "which this credential cannot read, so nothing can say which feature it is in; no run source of this stack binds it"})
		default:
			return fmt.Errorf("check which feature the %s %s is in: %w — nothing was written", e.Kind, e.ID, got.err)
		}
	}
	return refuseRunsOutside(f, outside)
}

// runTargetOf reads the feature (and name) of a run edge's target.
func runTargetOf(ctx context.Context, client *api.Client, kind, id string) (feature, name string, err error) {
	switch kind {
	case markers.KindWorkflow:
		wf, err := client.GetWorkflow(ctx, id)
		if err != nil {
			return "", "", err
		}
		return wf.FeatureID, wf.Title, nil
	case markers.KindAgent:
		agent, err := client.GetAgent(ctx, id)
		if err != nil {
			return "", "", err
		}
		return agent.FeatureID, agent.Name, nil
	}
	return "", "", fmt.Errorf("a %s is not something a folder runs", kind)
}

// workflowRunEdges are a workflow candidate's run edges: the workflows and
// agents validate RESOLVED for the posted files, each located on the line of
// the posted files that names it (or named by its marker when no line does).
func workflowRunEdges(files []api.ValidateFile, result *api.ValidateResult) []runEdge {
	if result == nil {
		return nil
	}
	resolved := map[string]map[string]bool{markers.KindWorkflow: {}, markers.KindAgent: {}}
	for _, id := range result.Resolved.WorkflowIDs {
		resolved[markers.KindWorkflow][id] = true
	}
	for _, id := range result.Resolved.AgentIDs {
		resolved[markers.KindAgent][id] = true
	}
	var out []runEdge
	located := map[string]bool{}
	for _, file := range files {
		for i, line := range strings.Split(file.Content, "\n") {
			for _, o := range markers.Scan(line) {
				kind := o.Family.Kind
				if !resolved[kind][o.Arg] {
					continue
				}
				out = append(out, runEdge{Where: fmt.Sprintf("%s:%d", file.Path, i+1), Kind: kind, ID: o.Arg})
				located[kind+"\x00"+o.Arg] = true
			}
		}
	}
	for _, kind := range []string{markers.KindWorkflow, markers.KindAgent} {
		for _, id := range sortedKeys(resolved[kind]) {
			if !located[kind+"\x00"+id] {
				out = append(out, runEdge{Where: fmt.Sprintf("{{ %s('%s') }}", kind, id), Kind: kind, ID: id})
			}
		}
	}
	// FAIL CLOSED: a workflow or agent the caller cannot reach is DROPPED from
	// `resolved` and reported as a finding instead (the server's binding legs),
	// so an edge read off `resolved` alone would be no edge at all. Each such
	// marker is an edge too, and gateRunEdges places it like any other — an
	// unreadable one is outside, unless this stack binds or manages it.
	for _, finding := range result.Findings {
		kind := unresolvedRunEdgeKinds[finding.Code]
		if kind == "" {
			continue
		}
		for _, o := range markers.Scan(finding.Marker) {
			if o.Family.Kind != kind || o.Positional || located[kind+"\x00"+o.Arg] {
				continue
			}
			located[kind+"\x00"+o.Arg] = true
			where := finding.Path
			if where == "" {
				where = finding.Marker
			}
			out = append(out, runEdge{Where: where, Kind: kind, ID: o.Arg})
		}
	}
	return out
}

// unresolvedRunEdgeKinds maps validate's finding code for a run edge it could
// not resolve to the edge's kind (rworkflow FindingUnresolvedWorkflow /
// FindingUnresolvedAgent — a wire contract: added to, never renamed).
var unresolvedRunEdgeKinds = map[string]string{
	"unresolved_workflow": markers.KindWorkflow,
	"unresolved_agent":    markers.KindAgent,
}

// tooManyRunEdges names the run-edge legs validate did not resolve at all
// because the code names more than a workflow may bind (its too_many_bindings
// finding, whose path is the leg's label): none of their edges reached
// `resolved`, so nothing can say where they point.
func tooManyRunEdges(f *folder, result *api.ValidateResult) error {
	if result == nil {
		return nil
	}
	var outside []gateOutside
	for _, finding := range result.Findings {
		if finding.Code != "too_many_bindings" || (finding.Path != "workflowIDs" && finding.Path != "agentIDs") {
			continue
		}
		outside = append(outside, gateOutside{Verb: "runs", Where: finding.Path, Target: "more workflows or agents than a workflow may bind",
			Why: "which the server did not resolve, so nothing can say which feature any of them is in"})
	}
	return refuseRunsOutside(f, outside)
}

// gateWorkflowCandidate is the whole gate over one workflow candidate — the
// files posted to validate and what it answered: the writes, then the run
// edges.
func gateWorkflowCandidate(ctx context.Context, client *api.Client, f *folder, files []api.ValidateFile, result *api.ValidateResult) error {
	if err := gateWorkflowTargets(f, result); err != nil {
		return err
	}
	if f.adoptedHere() {
		if err := tooManyRunEdges(f, result); err != nil {
			return err
		}
	}
	return gateRunEdges(ctx, client, f, workflowRunEdges(files, result))
}

// noteGate records one gate note on the folder — once — and prints it, unless
// `sync apply` collects it for the folder's line.
func (f *folder) noteGate(note string) {
	for _, n := range f.gateNotes {
		if n == note {
			return
		}
	}
	f.gateNotes = append(f.gateNotes, note)
	if !f.gateNotesCollected {
		fmt.Fprintf(os.Stderr, "  Note: %s.\n", indentContinuation(note, "          "))
	}
}

// indentContinuation indents every line of a multi-line note or warning after
// its first by pad, so the first line — what happened and what to do — stands
// out and the explanation reads as belonging to it.
func indentContinuation(s, pad string) string {
	return strings.ReplaceAll(s, "\n", "\n"+pad)
}

// noteBoundOutside is the note an ADMITTED bind earns when it lands outside
// the stack's feature: not a refusal — the stack bound it on
// purpose — but worth saying, and worth saying loudly when another stack of
// the same folder binds the very same row, which is what a bind copied from
// one stack to another looks like. feature is the target's feature when known
// ("" when it is not); verb is "writes" or "runs".
func (f *folder) noteBoundOutside(auth writeAuthority, where, verb, target, alias, id, feature string) {
	var same []string
	for _, name := range f.Manifest.StackNames() {
		if name != f.Stack && f.Manifest.Stacks[name].Bind[alias] == id {
			same = append(same, fmt.Sprintf("%q", name))
		}
	}
	outside := feature != "" && feature != auth.featureID
	if !outside && len(same) == 0 {
		return
	}
	note := fmt.Sprintf("%s %s %s", where, verb, target)
	if outside {
		note += fmt.Sprintf(" in feature %s, outside %s's feature %s", feature, describeStackFor(f.Stack), orUnknown(auth.featureID))
	}
	admitted := fmt.Sprintf("admitted because %q is bound for this stack on purpose", alias)
	if len(same) == 0 {
		f.noteGate(note + " — " + admitted)
		return
	}
	// The case to act on, so the action leads and the reason follows on a
	// line of its own.
	f.noteGate(fmt.Sprintf("%s, which stack %s binds too — check the bind for %q is meant for this stack.\nIt was %s; stack %s binds the same row, so this stack %s what that one does",
		note, strings.Join(same, ", "), alias, admitted, strings.Join(same, ", "), verb))
}

// noteManagedOutside is noteBoundOutside's twin for a row an ADDRESS
// dependency resolved to: admitted (this repository manages it), and said when
// it lives outside the stack's feature — the folder that manages it deploys
// this stack somewhere else, which a stack block copied between stacks does.
func (f *folder) noteManagedOutside(auth writeAuthority, where, verb, target, alias, feature string) {
	f.noteGate(fmt.Sprintf("%s %s %s in feature %s, outside %s's feature %s — check that the folder managing it means to deploy %s there.\nIt was admitted because %q names it by address, as a row this repository manages",
		where, verb, target, feature, describeStackFor(f.Stack), orUnknown(auth.featureID), describeStackFor(f.Stack), alias))
}

// describeFeatureOf names where a readable target lives.
func describeFeatureOf(featureID string) string {
	if featureID == "" {
		return "in no feature this credential can name"
	}
	return "in feature " + featureID
}

// gateWorkflowTargets is the workflow rule over validate's writeTargets.
func gateWorkflowTargets(f *folder, result *api.ValidateResult) error {
	if result == nil || result.WriteTargets == nil {
		// An instance too old to report write targets. Reading the absence as
		// "writes nothing" would wave every write through, so it is refused —
		// the one answer the gate can give without a scanner of its own.
		return &managedRefusal{Reason: syncReasonNoWriteTargets, Detail: fmt.Sprintf(
			"this Ronja instance (%s) does not report write targets — POST /workflow/validate answered no writeTargets — so it is older than this CLI. %s has a folder id, so every write it deploys is checked against %s's feature, and without that answer nothing here can vouch for this code. Nothing was written. Deploy once the instance is upgraded, use a `ronja` that matches it, or run `ronja sync unmanage --stack %s` to deploy this folder the classic way",
			f.Resolved.URL, wfdir.ManifestName, describeStackFor(f.Stack), f.Stack)}
	}
	auth := f.writeAuthorityOf()
	var outside []gateOutside
	for _, t := range result.WriteTargets {
		if o, refused := judgeWorkflowTarget(auth, t); refused {
			outside = append(outside, o)
			continue
		}
		if t.TargetKind != api.WriteTargetTable && t.TargetKind != api.WriteTargetCodex {
			continue
		}
		kind := markers.KindTable
		if t.TargetKind == api.WriteTargetCodex {
			kind = markers.KindCodex
		}
		if alias, ok := auth.boundAs(kind, t.TargetID); ok && t.TargetID != "" {
			feature := ""
			if t.Readable {
				feature = t.TargetFeatureID
			}
			f.noteBoundOutside(auth, workflowTargetWhere(t), "writes", fmt.Sprintf("the %s %s", t.TargetKind, t.TargetID), alias, t.TargetID, feature)
		}
	}
	return refuseWritesOutside(f, outside)
}

// workflowTargetWhere is where a validate write target sits: file, line and
// the call it is an argument of.
func workflowTargetWhere(t api.ValidateWriteTarget) string {
	where := fmt.Sprintf("%s:%d", t.Path, t.Line)
	if t.Call != "" {
		where += fmt.Sprintf(" (tools.%s)", t.Call)
	}
	return where
}

// judgeWorkflowTarget decides one validate write target.
func judgeWorkflowTarget(auth writeAuthority, t api.ValidateWriteTarget) (gateOutside, bool) {
	where := workflowTargetWhere(t)
	switch t.TargetKind {
	case api.WriteTargetTable, api.WriteTargetCodex:
		kind := markers.KindTable
		if t.TargetKind == api.WriteTargetCodex {
			kind = markers.KindCodex
		}
		if t.TargetKind == api.WriteTargetTable && t.NameForm {
			// A table by NAME: the runtime creates (or reuses) it in the run's
			// own feature, which is this stack's.
			return gateOutside{}, false
		}
		if t.TargetID == "" {
			return gateOutside{Where: where, Target: fmt.Sprintf("the %s %q", t.TargetKind, t.Arg),
				Why: "which names no row this ronja can place in a feature"}, true
		}
		if _, ok := auth.boundAs(kind, t.TargetID); ok {
			return gateOutside{}, false
		}
		if t.Readable && t.TargetFeatureID != "" && t.TargetFeatureID == auth.featureID {
			return gateOutside{}, false
		}
		target := fmt.Sprintf("the %s %s", t.TargetKind, t.TargetID)
		if t.Readable && t.TargetName != "" {
			target = fmt.Sprintf("the %s %s (%s)", t.TargetKind, t.TargetID, t.TargetName)
		}
		if !t.Readable {
			return gateOutside{Where: where, Target: target,
				Why: "which this credential cannot read, so nothing can say which feature it is in; no write source of this stack binds it"}, true
		}
		return gateOutside{Where: where, Target: target, Feature: t.TargetFeatureID,
			Why: describeFeatureOf(t.TargetFeatureID) + ", and no write source of this stack binds it"}, true
	case api.WriteTargetFolder:
		path := t.TargetPath
		if norm, ok := wfdir.NormalizeFolderPath(path); ok && norm != "" {
			path = norm
		}
		if _, ok := auth.boundAs(markers.KindFolder, path); ok && path != "" {
			return gateOutside{}, false
		}
		return gateOutside{Where: where, Target: fmt.Sprintf("into the folder %s", orUnknown(path)),
			Why: "a path every feature of the organization shares, which only a folder write source (\"kind\": \"folder\", \"write\": true) bound to it in this stack admits"}, true
	}
	return gateOutside{Where: where, Target: fmt.Sprintf("a %q target %q", t.TargetKind, t.Arg),
		Why: "a kind of write this ronja does not know how to place, so it cannot vouch for it — upgrade the CLI"}, true
}

// workflowGateInput is the validate request a push of the LOCAL folder makes:
// every file, resolved through the stack's codec, the folder's declared
// parameters and runtime. live is the row the push would write (nil for a
// create).
func workflowGateInput(f *folder, local map[string]string, params []api.WorkflowParameter, live *api.Workflow) api.ValidateInput {
	return api.ValidateInput{
		FeatureID:      f.Binding.FeatureID,
		Entrypoint:     f.Manifest.Entrypoint,
		Parameters:     params,
		Files:          validateFilesOf(f.Codec, local),
		RuntimeVersion: f.Manifest.RuntimeForValidate(live),
		Language:       languageOnTheWire(f.Manifest),
	}
}

// gateWorkflowRow is the gate over a STORED draft — what `wf publish` would
// commit and `wf test` would run, which the web app may have edited since the
// folder pushed it — or the live row `wf run` runs. The files go up exactly as
// the row holds them. ⚠️ The commit or run is a LATER request: an edit made in
// between is not seen. See BL-b3c7.
func gateWorkflowRow(ctx context.Context, client *api.Client, f *folder, row *api.Workflow) error {
	if !f.adoptedHere() || row == nil {
		return nil
	}
	files, err := client.ListWorkflowFiles(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("read the files of %s to check where they write: %w", row.ID, err)
	}
	in := api.ValidateInput{
		FeatureID:      f.Binding.FeatureID,
		Entrypoint:     row.Entrypoint,
		Parameters:     row.Parameters,
		RuntimeVersion: row.RuntimeVersion,
		Language:       rowLanguageOnTheWire(row),
		Files:          make([]api.ValidateFile, 0, len(files)),
	}
	for _, file := range files {
		in.Files = append(in.Files, api.ValidateFile{Path: file.Path, Content: file.Content})
	}
	sort.Slice(in.Files, func(i, j int) bool { return in.Files[i].Path < in.Files[j].Path })
	result, err := client.ValidateWorkflowFiles(ctx, in)
	if err != nil {
		return fmt.Errorf("check where %s writes (POST /workflow/validate): %w — nothing was written", row.ID, err)
	}
	return gateWorkflowCandidate(ctx, client, f, in.Files, result)
}

// rowLanguageOnTheWire is languageOnTheWire for a stored row: the row's own
// language, not the folder's, since the row is what runs. Without it the
// server scans a js row as Python, finds no write in main.js, and the gate
// passes writes it never saw.
func rowLanguageOnTheWire(row *api.Workflow) string {
	if row.Language == wfdir.LanguageJS {
		return wfdir.LanguageJS
	}
	return ""
}

// gateAppAccess is the data-app rule: every codex the access block grants is
// written by the app (its token's codex grant covers upsert and delete), and
// every workflow and agent it allows is run by it. access is the WIRE form —
// ids, after the codec.
func gateAppAccess(ctx context.Context, client *api.Client, f *folder, access api.DataAppAccess) error {
	if !f.adoptedHere() {
		return nil
	}
	if err := gateAppCodexes(ctx, client, f, access); err != nil {
		return err
	}
	var edges []runEdge
	for i, id := range access.AllowedWorkflowIDs {
		edges = append(edges, runEdge{Where: fmt.Sprintf("%s → access.allowedWorkflowIDs[%d]", wfdir.ManifestName, i), Kind: markers.KindWorkflow, ID: id})
	}
	for i, id := range access.AllowedAgentIDs {
		edges = append(edges, runEdge{Where: fmt.Sprintf("%s → access.allowedAgentIDs[%d]", wfdir.ManifestName, i), Kind: markers.KindAgent, ID: id})
	}
	return gateRunEdges(ctx, client, f, edges)
}

// gateAppCodexes is gateAppAccess's write half: the codexes the app writes.
func gateAppCodexes(ctx context.Context, client *api.Client, f *folder, access api.DataAppAccess) error {
	if len(access.AllowedCodexIDs) == 0 {
		return nil
	}
	auth := f.writeAuthorityOf()
	var outside []gateOutside
	for i, id := range access.AllowedCodexIDs {
		where := fmt.Sprintf("%s → access.allowedCodexIDs[%d]", wfdir.ManifestName, i)
		if alias, ok := auth.boundAs(markers.KindCodex, id); ok {
			if codex, err := client.GetCodex(ctx, id); err == nil {
				f.noteBoundOutside(auth, where, "writes", "the codex "+id, alias, id, codex.FeatureID)
			}
			continue
		}
		codex, err := client.GetCodex(ctx, id)
		switch {
		case err == nil && codex.FeatureID != "" && codex.FeatureID == auth.featureID:
			continue
		case err == nil:
			outside = append(outside, gateOutside{Where: where, Target: "the codex " + id,
				Feature: codex.FeatureID, Why: describeFeatureOf(codex.FeatureID) + ", and no write source of this stack binds it"})
		case api.StatusOf(err) == 400 || api.StatusOf(err) == 403 || api.StatusOf(err) == 404:
			outside = append(outside, gateOutside{Where: where, Target: "the codex " + id,
				Why: "which this credential cannot read, so nothing can say which feature it is in; no write source of this stack binds it"})
		default:
			return fmt.Errorf("check which feature the codex %s is in: %w — nothing was written", id, err)
		}
	}
	return refuseWritesOutside(f, outside)
}

// gateAutomationReferences is the automation rule: every TABLE in an agent
// action's reference set may be written by the headless run, and so may every
// table of every FEATURE there. files are resolved (ids), keyed by path.
func gateAutomationReferences(ctx context.Context, client *api.Client, f *folder, files map[string]*automationFile) error {
	if !f.adoptedHere() {
		return nil
	}
	auth := f.writeAuthorityOf()
	var outside []gateOutside
	for _, path := range sortedKeys(files) {
		file := files[path]
		if file == nil || file.References == nil {
			continue
		}
		for i, ref := range *file.References {
			kind := automationReferenceKinds[ref.Kind]
			if (kind != markers.KindTable && kind != markers.KindFeature) || ref.ResourceID == "" {
				continue
			}
			id := ref.ResourceID
			where := fmt.Sprintf("%s → references[%d]", path, i)
			if alias, ok := auth.boundAs(kind, id); ok {
				switch kind {
				case markers.KindFeature:
					f.noteBoundOutside(auth, where, "writes", "every table of the feature "+id, alias, id, id)
				default:
					if table, err := client.GetTable(ctx, id); err == nil {
						f.noteBoundOutside(auth, where, "writes", "the table "+id, alias, id, table.FeatureID)
					}
				}
				continue
			}
			if kind == markers.KindFeature {
				// "@feature" was resolved to the stack's own feature before
				// this ran, so it passes here; any other feature is one whose
				// every table the run is given.
				if id == auth.featureID {
					continue
				}
				outside = append(outside, gateOutside{Where: where, Target: "every table of the feature " + id, Feature: id,
					Why: "a feature an agent run given it may write throughout, other than this stack's own, and no write source (\"kind\": \"feature\", \"write\": true) of this stack binds it"})
				continue
			}
			table, err := client.GetTable(ctx, id)
			switch {
			case err == nil && table.Kind == "metric":
				// A metric is a recipe: no tool writes one, so the reference is
				// a read (the inventory narrows it back to read the same way).
				continue
			case err == nil && table.FeatureID != "" && table.FeatureID == auth.featureID:
				continue
			case err == nil:
				outside = append(outside, gateOutside{Where: where, Target: fmt.Sprintf("the table %s (%s)", id, table.Name),
					Feature: table.FeatureID, Why: describeFeatureOf(table.FeatureID) + ", which an agent run given it may write, and no write source of this stack binds it"})
			case api.StatusOf(err) == 400 || api.StatusOf(err) == 403 || api.StatusOf(err) == 404:
				outside = append(outside, gateOutside{Where: where, Target: "the table " + id,
					Why: "which this credential cannot read, so nothing can say which feature it is in; no write source of this stack binds it"})
			default:
				return fmt.Errorf("check which feature the table %s is in: %w — nothing was written", id, err)
			}
		}
	}
	if err := refuseWritesOutside(f, outside); err != nil {
		return err
	}
	return gateRunEdges(ctx, client, f, automationRunEdges(files))
}

// automationRunEdges are what the automation files RUN: a workflow action's
// workflow, a saved_agent action's agent, and every `workflow` reference.
// files are resolved (ids); an action a file does not declare is not sent, so
// it is not checked.
func automationRunEdges(files map[string]*automationFile) []runEdge {
	var out []runEdge
	for _, path := range sortedKeys(files) {
		file := files[path]
		if file == nil {
			continue
		}
		if a := file.Action; a != nil {
			switch a.Kind {
			case api.ActionKindWorkflow:
				if id := a.Config.WorkflowID; id != nil && *id != "" {
					out = append(out, runEdge{Where: path + " → action.config.workflowID", Kind: markers.KindWorkflow, ID: *id})
				}
			case api.ActionKindSavedAgent:
				if id := a.Config.AgentID; id != nil && *id != "" {
					out = append(out, runEdge{Where: path + " → action.config.agentID", Kind: markers.KindAgent, ID: *id})
				}
			}
		}
		if file.References == nil {
			continue
		}
		for i, ref := range *file.References {
			if automationReferenceKinds[ref.Kind] == markers.KindWorkflow && ref.ResourceID != "" {
				out = append(out, runEdge{Where: fmt.Sprintf("%s → references[%d]", path, i), Kind: markers.KindWorkflow, ID: ref.ResourceID})
			}
		}
	}
	return out
}

// gateTableDocsSidecars is the rule over a pipeline folder's docs sidecars:
// `tables/<alias>.json` writes its table's description and column prose, so the
// table is a write target like any other — in the stack's own feature, a row
// this repository manages in the stack (an address dependency resolved it), or
// bound IN THIS STACK by a write-capable data source ("write": true). A sidecar
// that cannot be used as it stands (Problem), or declares nothing, writes
// nothing and is not asked about. Reads only.
func gateTableDocsSidecars(ctx context.Context, client *api.Client, f *folder, sidecars []tableDocsSidecar) error {
	if !f.adoptedHere() {
		return nil
	}
	auth := f.writeAuthorityOf()
	var outside []gateOutside
	for _, file := range sidecars {
		if file.Problem != "" || file.TableID == "" || file.Docs.Empty() {
			continue
		}
		id := file.TableID
		if alias, ok := auth.managed[markers.KindTable][id]; ok {
			if feature := auth.managedFeature[alias]; feature != "" && feature != auth.featureID {
				f.noteManagedOutside(auth, file.Path, "documents", "the table "+id, alias, feature)
			}
			continue
		}
		table, err := client.GetTable(ctx, id)
		if alias, ok := auth.boundAs(markers.KindTable, id); ok {
			if err == nil {
				f.noteBoundOutside(auth, file.Path, "documents", "the table "+id, alias, id, table.FeatureID)
			}
			continue
		}
		switch status := api.StatusOf(err); {
		case err == nil && table.FeatureID != "" && table.FeatureID == auth.featureID:
			continue
		case err == nil:
			outside = append(outside, gateOutside{Verb: "documents", Where: file.Path, Target: fmt.Sprintf("the table %s (%s)", id, table.Name),
				Feature: table.FeatureID, Why: describeFeatureOf(table.FeatureID) + ", and no write source of this stack binds it"})
		case status == 400 || status == 403 || status == 404:
			outside = append(outside, gateOutside{Verb: "documents", Where: file.Path, Target: "the table " + id,
				Why: "which this credential cannot read, so nothing can say which feature it is in; no write source of this stack binds it"})
		default:
			return fmt.Errorf("check which feature the table %s is in: %w — nothing was written", id, err)
		}
	}
	return refuseWritesOutside(f, outside)
}

// planWriteGate is `sync apply`'s plan-time gate: the folder's writes and run
// edges exactly as its push would send them, checked before the run writes
// anything for it.
//
//   - A folder that CREATES its row is gated here, and not only inside the
//     push: the create is the first thing written, and a folder whose code
//     writes outside this stack's feature must not leave an empty, claimed row
//     behind on its way to being refused.
//   - A pipeline folder's docs sidecars are gated here whatever it creates:
//     they are its only writes a stack's feature does not contain by
//     construction (every table it builds is created in that feature).
//   - A DRY RUN gates every folder, update or create: the push that gates an
//     update never runs in one, and a preview that said `applied` for a folder
//     the apply then refuses would make a green pull-request check of a deploy
//     that fails after the merge.
//
// Every request here is a read (validate is a check, not a save). A module is
// marker-free and has nothing to gate; an automation's real apply is gated by
// its push, which creates and writes in one step.
func planWriteGate(ctx context.Context, f *folder, files map[string]string, decisions []applyDecision) (reason, detail string) {
	if !f.adoptedHere() {
		return "", ""
	}
	creates := false
	for _, d := range decisions {
		if d.Creates() {
			creates = true
		}
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	var err error
	switch f.Kind.Name {
	case wfdir.KindWorkflow:
		if !creates && !flagSyncDryRun {
			return "", ""
		}
		var params []api.WorkflowParameter
		if f.Manifest.ManagesParameters() {
			params = f.wireParameters()
		}
		in := workflowGateInput(f, files, params, nil)
		var result *api.ValidateResult
		result, err = client.ValidateWorkflowFiles(ctx, in)
		if err == nil {
			err = gateWorkflowCandidate(ctx, client, f, in.Files, result)
		}
	case wfdir.KindDataApp:
		if (creates || flagSyncDryRun) && f.Manifest.ManagesAccess() {
			err = gateAppAccess(ctx, client, f, f.declaredAccess())
		}
	case wfdir.KindPipeline:
		var sidecars []tableDocsSidecar
		if sidecars, err = readTableDocsSidecars(f.Root); err == nil {
			err = gateTableDocsSidecars(ctx, client, f, resolveTableDocsSidecars(f, sidecars))
		}
	case wfdir.KindAutomation:
		if flagSyncDryRun {
			if resolved := automationGateFiles(f, files); resolved != nil {
				err = gateAutomationReferences(ctx, client, f, resolved)
			}
		}
	}
	if err == nil {
		return "", ""
	}
	if r, ok := asManagedRefusal(err); ok {
		return r.Reason, r.Detail
	}
	return syncReasonLookupFailed, fmt.Sprintf("could not check where this folder writes before deploying it: %v — nothing was written", err)
}

// automationGateFiles are an automation folder's files parsed and resolved as
// its push resolves them, for the plan-time gate — nil when any of them does
// not parse or resolve, which the push itself refuses before it sends anything.
//
// An address dependency still PENDING (a fresh stack: its row is created by
// the same run) resolves to its own alias here, the spelling the gate admits as
// a row this repository will manage (pendingAddress) — without it, one pending
// reference would leave the whole folder ungated in a dry run.
func automationGateFiles(f *folder, local map[string]string) map[string]*automationFile {
	parsed, problems := parseAutomationFolder(local)
	if len(problems) > 0 {
		return nil
	}
	codec := f.Codec
	if len(f.addressPending) > 0 {
		bind := maps.Clone(f.codecBind())
		if bind == nil {
			bind = map[string]string{}
		}
		for alias := range f.addressPending {
			if _, bound := bind[alias]; !bound {
				bind[alias] = alias
			}
		}
		codec = newAliasCodec(f.Manifest.Dependencies, bind)
	}
	out := make(map[string]*automationFile, len(parsed))
	for _, path := range sortedPaths(local) {
		file := *parsed[path]
		if err := resolveAutomationRefs(&file, codec, f.Stack, f.Binding.FeatureID); err != nil {
			return nil
		}
		out[path] = &file
	}
	return out
}
