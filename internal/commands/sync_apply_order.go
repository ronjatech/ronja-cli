package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A FRESH STACK IN ONE APPLY.
//
// A folder names another folder's row BY ADDRESS, resolved through that
// folder's claim. In a stack nothing has been applied to, no claim exists yet,
// so every such address used to refuse address_unresolved and standing a stack
// up took apply, bind, commit, apply. `sync apply` now:
//
//  1. PLANS every folder, deferring an address whose only problem is that the
//     other folder holds no claim at it yet (addressPending) — and keeps the
//     deferral only when the same run deploys that folder, and that folder
//     holds a unit at that address (resolveDeferredAddresses). Anything else
//     still refuses, as before.
//  2. RUNS the folders in DEPENDENCY order (applySequence), not path order,
//     resolving each deferred address in memory to the row the run made
//     (addressOverlay) just before the folder that needs it runs:
//
//     - modules, which reference nothing;
//     - pipelines, in the order their cross-folder table reads impose — a
//     table read by address is a build edge, because a draft builds
//     against its inputs' data and an input that does not exist yet cannot
//     be read. A cycle there is refused, as topoOrder refuses one inside a
//     folder;
//     - PASS ONE: every workflow another workflow of the run names by address,
//     that does not exist yet and whose own addresses all resolve by now, is
//     CREATED with its claim and nothing else (passOne, createWorkflowFirst);
//     - PASS TWO: every workflow's code is written and published. A
//     `{{ workflow }}` marker then always names a row that exists, so a run
//     edge needs no order at all — A→B→A included;
//     - data apps, then automations: nothing a pipeline or a workflow writes
//     names either, and everything they name exists by now.
//
// A failure part way is safe: every row is created WITH its claim, so the
// next apply finds it through the server's state and writes it rather than
// creating a second one.

// The order apply deploys in, as the report names it.
const applyOrderDependency = "dependency"

// deferrablePending reports whether the alias pre-flight's refusals are ALL
// addresses another folder has no claim at yet — the one refusal a run that
// deploys that folder can satisfy — and returns them.
func deferrablePending(f *folder, report aliasReport) ([]addressPending, bool) {
	if len(report.Address) == 0 || len(report.Address) != len(report.Refusals) {
		return nil, false
	}
	out := make([]addressPending, 0, len(report.Address))
	for _, r := range report.Address {
		p, ok := f.addressPending[r.Alias]
		if !ok {
			return nil, false
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, true
}

// resolveDeferredAddresses keeps a deferred address only when a folder this run
// will deploy holds a unit at that address, and refuses every other plan that
// deferred one — to a fixed point, because refusing a folder takes its units
// away from every folder waiting on them.
func resolveDeferredAddresses(planned []plannedFolder) {
	for {
		held := map[string]bool{}
		for i := range planned {
			p := &planned[i]
			if p.f == nil || !p.f.adoptedHere() {
				continue
			}
			for _, d := range p.decisions {
				held[overlayKey(p.f.Binding.FeatureID, p.f.Manifest.ID, managedAddressOf(p.f.Kind, d.Path))] = true
			}
		}
		changed := false
		for i := range planned {
			p := &planned[i]
			if p.f == nil || len(p.pending) == 0 {
				continue
			}
			for _, d := range p.pending {
				if held[overlayKey(d.FeatureID, d.FolderID, d.Address)] {
					continue
				}
				p.line.Verdict, p.line.Reason = verdictRefused, syncReasonAddressUnresolved
				p.line.Detail = fmt.Sprintf("%s\n  This run does not deploy that folder's row at %s either — the folder is not under this directory, or it is refused above — so there is nothing to resolve it to",
					p.pendingDetail, describeAddressKey(d.Address))
				p.f = nil
				changed = true
				break
			}
		}
		if !changed {
			return
		}
	}
}

// tableReadRoots are the pipeline roots this plan reads a table from by
// address, resolved or deferred — each a build edge.
func (p *plannedFolder) tableReadRoots() []string {
	var out []string
	if p.f != nil {
		for _, e := range p.f.crossFolderTableReads() {
			out = append(out, realPath(e.FromRoot))
		}
	}
	for _, d := range p.pending {
		if d.Kind == markers.KindTable && strings.HasSuffix(d.Address, ".sql") {
			out = append(out, realPath(d.FromRoot))
		}
	}
	return out
}

// applySequence is the order the planned folders run in, as groups: modules;
// pipelines in build order; workflows; data apps; automations; and the rest
// (folders ruled out before they loaded, whose lines are already final). Within
// a group, path order. A cycle of cross-folder table reads refuses every
// pipeline on it.
func applySequence(planned []plannedFolder) (modules, pipelines, workflows, apps, automations, rest []int) {
	for i := range planned {
		switch planned[i].line.Kind {
		case wfdir.KindModule:
			modules = append(modules, i)
		case wfdir.KindPipeline:
			pipelines = append(pipelines, i)
		case wfdir.KindWorkflow:
			workflows = append(workflows, i)
		case wfdir.KindDataApp:
			apps = append(apps, i)
		case wfdir.KindAutomation:
			automations = append(automations, i)
		default:
			rest = append(rest, i)
		}
	}
	return modules, pipelineBuildOrder(planned, pipelines), workflows, apps, automations, rest
}

// pipelineBuildOrder orders the pipeline folders so a folder reading another's
// table by address runs after it (Kahn's algorithm, ties broken by path), and
// refuses every folder left on a cycle. Folders that will not run keep their
// place at the end; they write nothing.
func pipelineBuildOrder(planned []plannedFolder, pipelines []int) []int {
	byRoot := map[string]int{}
	for _, i := range pipelines {
		if planned[i].f != nil {
			byRoot[realPath(planned[i].line.Root)] = i
		}
	}
	after := map[int][]int{} // i must run before each of after[i]
	needs := map[int]int{}
	for _, i := range pipelines {
		if planned[i].f == nil {
			continue
		}
		seen := map[int]bool{}
		for _, root := range planned[i].tableReadRoots() {
			j, ok := byRoot[root]
			if !ok || j == i || seen[j] {
				continue
			}
			seen[j] = true
			after[j] = append(after[j], i)
			needs[i]++
		}
	}
	var order, ready []int
	for _, i := range pipelines {
		if planned[i].f != nil && needs[i] == 0 {
			ready = append(ready, i)
		}
	}
	done := map[int]bool{}
	for len(ready) > 0 {
		sort.Ints(ready)
		i := ready[0]
		ready = ready[1:]
		order = append(order, i)
		done[i] = true
		for _, k := range after[i] {
			needs[k]--
			if needs[k] == 0 {
				ready = append(ready, k)
			}
		}
	}
	var cycle []int
	for _, i := range pipelines {
		if planned[i].f != nil && !done[i] {
			cycle = append(cycle, i)
		}
	}
	if len(cycle) > 0 {
		names := make([]string, 0, len(cycle))
		for _, i := range cycle {
			names = append(names, planned[i].line.Path)
		}
		for _, i := range cycle {
			p := &planned[i]
			p.line.Verdict, p.line.Reason = verdictRefused, syncReasonCyclicDependency
			p.line.Detail = fmt.Sprintf("these pipeline folders read each other's tables in a loop (%s), so there is no order to build them in — a table read by address is a build edge, and one of the reads has to go", strings.Join(names, ", "))
			p.f = nil
		}
		order = append(order, cycle...)
	}
	for _, i := range pipelines {
		if !done[i] && planned[i].f == nil && !containsIndex(order, i) {
			order = append(order, i)
		}
	}
	return order
}

func containsIndex(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// settlePlanned is the execute pass's managed step for one folder (moves,
// removals, lazy claims), once — pass one runs it before a create, and the
// folder's own run then skips it. False when it refused the folder, whose line
// it has finished.
func settlePlanned(ctx context.Context, plan *plannedFolder) bool {
	if plan.settled {
		return true
	}
	plan.settled = true
	line := &plan.line
	notes, err := plan.f.settleManaged(ctx)
	line.Notes = append(line.Notes, notes...)
	if err == nil {
		return true
	}
	switch r, ok := asManagedRefusal(err); {
	case ok:
		line.Verdict, line.Reason, line.Detail = verdictRefused, r.Reason, r.Detail
	case api.Unanswered(err) || api.IsTimeout(err):
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonUnanswered, err.Error()
	default:
		line.Verdict, line.Reason, line.Detail = verdictRefused, syncReasonNotDeployed, err.Error()
	}
	plan.f = nil
	return false
}

// passOne decides which workflow rows pass one creates, once the modules and
// pipelines have run: a workflow another READY workflow of this run names by
// address, that does not exist yet, and is ready itself. READY means every
// address it waits on resolves by now — a table or module in memory (overlay),
// a workflow because pass one will create it — decided to a fixed point, so a
// workflow waiting on a row that never appeared (a pipeline that failed) is
// not created, and neither is a workflow only it names. Only a workflow names
// a workflow before pass two writes code: an app or an automation naming one
// runs after every workflow is live, so it needs nothing from pass one.
func passOne(planned []plannedFolder, workflows []int, overlay *addressOverlay) map[int]bool {
	key := func(p *plannedFolder) string { return overlayKey(p.f.Binding.FeatureID, p.f.Manifest.ID, "") }
	ready := map[string]bool{}
	creates := map[string]int{}
	for _, i := range workflows {
		p := &planned[i]
		if p.f == nil || !p.f.adoptedHere() {
			continue
		}
		ok := true
		for _, d := range p.pending {
			if _, found := overlay.lookup(d.FeatureID, d.FolderID, d.Address); !found && d.Kind != markers.KindWorkflow {
				ok = false
			}
		}
		if ok {
			ready[key(p)] = true
		}
		if workflowApplyDecision(p.f).Creates() {
			creates[key(p)] = i
		}
	}
	for changed := true; changed; {
		changed = false
		for _, i := range workflows {
			p := &planned[i]
			if p.f == nil || !ready[key(p)] {
				continue
			}
			for _, d := range p.pending {
				if d.Kind != markers.KindWorkflow {
					continue
				}
				target := overlayKey(d.FeatureID, d.FolderID, "")
				if _, made := creates[target]; !made || !ready[target] {
					delete(ready, key(p))
					changed = true
					break
				}
			}
		}
	}
	out := map[int]bool{}
	for _, i := range workflows {
		p := &planned[i]
		if p.f == nil || !ready[key(p)] {
			continue
		}
		for _, d := range p.pending {
			target := overlayKey(d.FeatureID, d.FolderID, "")
			if j, made := creates[target]; d.Kind == markers.KindWorkflow && made && ready[target] {
				out[j] = true
			}
		}
	}
	return out
}

// createWorkflowFirst is PASS ONE for one workflow folder passOne chose: when
// its row does not exist yet, create it — with its claim, and nothing an address could still be pending in — and
// record it, so every `{{ workflow }}` marker in pass two names a row that
// exists. Its code is written by the folder's own push in pass two, which
// reads the row the create seeded as the CLI's own half-finished first push
// (isUntouchedFirstPush) and goes on from there.
//
// A workflow passOne did not choose is left to its own push, which validates
// before it creates — so a folder that push would refuse leaves no empty row
// behind.
func createWorkflowFirst(ctx context.Context, plan *plannedFolder) {
	f := plan.f
	// An ADOPTED folder only: an address — the one thing pass one exists to
	// make resolvable — names only a folder that keeps server state, and a
	// classic folder's push creates its row exactly as it always has.
	if f == nil || f.Kind.Name != wfdir.KindWorkflow || !f.adoptedHere() || !workflowApplyDecision(f).Creates() {
		return
	}
	if !settlePlanned(ctx, plan) {
		return
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	featureID, err := featureIDFor(ctx, client, f, nil)
	if err == nil {
		var result pushResult
		_, err = createWorkflowRow(ctx, client, f, featureID, nil, &result)
		if err == nil && result.Created {
			plan.created++
		}
		if err == nil && result.RuntimePinned {
			plan.line.Notes = append(plan.line.Notes, runtimePinnedNote(result.RuntimeVersion))
		}
		// A row already held the address and was adopted: it exists, which is
		// all this pass is for.
		if adoptedOnCreate(err) {
			err = nil
		}
	}
	if err == nil {
		return
	}
	line := &plan.line
	switch r, ok := managedRefusalOf(f, err); {
	case ctx.Err() != nil:
		// Asked FIRST, as runPlannedFolder asks it: a create cut off by a
		// Ctrl-C may well have committed, which no refusal can claim.
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonInterrupted, "create the workflow before any code is written: "+err.Error()
	case ok:
		line.Verdict, line.Reason, line.Detail = verdictRefused, r.Reason, r.Detail
	case api.Unanswered(err) || api.IsTimeout(err):
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonUnanswered, "create the workflow before any code is written: "+err.Error()
	default:
		line.Verdict, line.Reason, line.Detail = verdictRefused, syncReasonNotDeployed, "create the workflow before any code is written: "+err.Error()
	}
	plan.f = nil
}
