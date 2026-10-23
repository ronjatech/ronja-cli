package commands

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The ADOPTED-folder steps of `sync apply`: which refusals are the server's
// state's own, relieving an anchorless automation already at desired, labelling
// the units the refresh decided, and noting a folder deployed unmanaged. Split
// from sync_apply.go.

// plannedFolder is one folder's answer BEFORE anything is written: the line as
// far as it can be filled in locally, and — only when the folder is cleared to
// run — the opened folder to run it on.
//
// The plan/execute split exists for --allow-create, whose promise is a number
// for the WHOLE TREE. See syncApplyTree.
// noteUnmanagedFolders puts a note on each folder this run deploys the classic
// way where the write gate could have applied: one whose stack is marked
// "unmanaged" in ronja.json, and — in a tree where some folder deploys this
// stack adopted — one with no folder id at all. Either is ONE committed line
// that turns the gate and the server's records off for the folder, and a diff
// containing it says nothing like "this may now write another feature"; the
// note makes the off-switch as visible as a bind. A tree with no adopted folder
// is the classic pattern throughout, and gets none.
func noteUnmanagedFolders(planned []plannedFolder) {
	adopted := false
	for i := range planned {
		if f := planned[i].f; f != nil && f.adoptedHere() {
			adopted = true
		}
	}
	for i := range planned {
		f := planned[i].f
		if f == nil || f.adoptedHere() || f.Stack == "" {
			continue
		}
		switch {
		case f.Manifest.Adopted() && f.Manifest.Stacks[f.Stack].Unmanaged:
			planned[i].line.Notes = append(planned[i].line.Notes, fmt.Sprintf(
				"stack %q is marked \"unmanaged\" in %s, so this folder is deployed unchecked — `ronja sync adopt --stack %s` turns the check back on.\nIt deploys the classic way: what it writes and runs is not checked against the stack's feature, and Ronja keeps no record of it; adopting turns both back on",
				f.Stack, wfdir.ManifestName, f.Stack))
		case !f.Manifest.Adopted() && adopted:
			planned[i].line.Notes = append(planned[i].line.Notes, fmt.Sprintf(
				"this folder has no folder id, so it is deployed unchecked while other folders of this run are adopted — `ronja sync adopt --stack %s` adopts it.\nIt deploys stack %q the classic way: what it writes and runs is not checked against the stack's feature, and Ronja keeps no record of it",
				f.Stack, f.Stack))
		}
	}
}

func isManagedRefusal(f *folder, err error) bool {
	_, ok := managedRefusalOf(f, err)
	return ok
}

// relieveAnchorlessAtDesired is "live equal to desired is never drift" for an
// ADOPTED automation folder's
// unanchored units: a bound row with no drift anchor whose live state already
// holds everything its file declares is not refused. The push writes nothing to
// it and records the anchor from the row it read (runAutomationPush's
// live-equals-desired leg), and the lock save records that anchor on the
// server.
//
// ⚠️ This is the PLAN's answer, from a read made before any folder executes,
// and it is not what keeps the write safe: the push runs with RequireAnchor,
// which asks the same question again of the row the push itself reads, and
// refuses no_drift_anchor if the row moved in between (a pause, say).
//
// Without it an adopted folder wedges for good: a create whose anchor write
// failed (or a crash between the two) leaves the claim's `applied` empty, a
// runner with no lock has no other anchor to read, and every apply refuses
// no_drift_anchor even though there is nothing to overwrite — while the remedy
// that refusal names, a per-folder push, is not something CI runs. A legacy
// folder keeps the refusal: its committed lock is where an anchor lives, and a
// person pushing the folder once records it there.
//
// Anything short of a clean answer leaves the unit refused: a file that does
// not parse or resolve, a row that cannot be read, or one that differs.
func relieveAnchorlessAtDesired(ctx context.Context, f *folder, files map[string]string, decisions []applyDecision) {
	if f.managed == nil || f.Kind.Name != wfdir.KindAutomation {
		return
	}
	var rows map[string]*api.Automation
	for i := range decisions {
		d := &decisions[i]
		if d.Action != applyActionUnanchored {
			continue
		}
		if rows == nil {
			var err error
			if rows, err = automationRowsOf(ctx, newClient(f.Resolved.URL, f.Resolved.Token), f); err != nil {
				return
			}
		}
		if held, refusal, ok := automationHeldLive(f, d.Path, files[d.Path], d.ID, rows); !ok || refusal != "" || !held {
			continue
		}
		d.Action, d.Reason = applyActionUpdate, ""
		d.Detail = fmt.Sprintf("no drift anchor is recorded for %s, and the row already holds everything this file declares — live equals desired is never drift, so apply records the anchor and writes nothing else", d.ID)
	}
}

// automationRowsOf is the push's one listing of the stack's feature, by id
// (runAutomationPush, step 5) — the rows automationHeldLive is asked of, so
// a row moved to another feature is not found, as the push does not find it,
// and a legacy row reads in the listing's shape rather than a GET's. A
// listing that did not fit one page is an error, as there.
func automationRowsOf(ctx context.Context, client *api.Client, f *folder) (map[string]*api.Automation, error) {
	if err := refuseBoundAutomationsWithNoFeature(f); err != nil {
		return nil, err
	}
	listed, err := client.ListAutomations(ctx, f.Binding.FeatureID)
	if err != nil {
		return nil, err
	}
	return automationRowsByID(listed), nil
}

// automationHeldLive asks what pushOneAutomation asks of a bound file before
// its first write, of rows (automationRowsOf): is the row in the stack's
// feature, does the file change nothing the server cannot patch
// (refuseUnpatchableAutomation), and does the row already hold everything the
// file declares (automationDelta) — so a push writes nothing to it.
//
// refusal is the push's own refusal of the file, when it would make one;
// held is meaningful only without one. ok is false when nothing can be said:
// the file does not parse or resolve.
func automationHeldLive(f *folder, path, body, id string, rows map[string]*api.Automation) (held bool, refusal string, ok bool) {
	parsed, err := parseAutomationFile(path, body)
	if err != nil {
		return false, "", false
	}
	file := *parsed
	if resolveAutomationRefs(&file, f.Codec, f.Stack, f.Binding.FeatureID) != nil {
		return false, "", false
	}
	row, present := rows[id]
	if !present {
		return false, fmt.Sprintf("binding broken: %s is not in feature %s any more (deleted, moved, or you lost access to it)", id, f.Binding.FeatureID), true
	}
	if err := refuseUnpatchableAutomation(&file, row); err != nil {
		return false, err.Error(), true
	}
	_, patch := automationDelta(&file, row)
	return patch.Empty(), "", true
}

func orFolder(address string) string { return cmp.Or(address, "the folder") }

func orNothing(id string) string { return cmp.Or(id, "nothing") }

// markManagedUnits relabels the units an ADOPTED folder's refresh decided:
// a unit whose lock entry the server's state rewrote is `adopt`, and a
// bound unit the server has no claim for says the apply claims it.
func markManagedUnits(f *folder, units []syncApplyUnitReport) {
	st := f.managed
	if st == nil {
		return
	}
	rewritten := map[string]managedRewrite{}
	for _, r := range st.rewrites {
		rewritten[r.Address] = r
	}
	unclaimed := map[string]bool{}
	for _, a := range f.managedBackfill() {
		unclaimed[a.Address] = true
	}
	for i := range units {
		u := &units[i]
		addr := managedAddressOf(f.Kind, u.Path)
		if u.Action != string(applyActionUpdate) && u.Action != string(applyActionUnanchored) {
			continue
		}
		if r, ok := rewritten[addr]; ok && r.To != "" && r.From != r.To {
			u.Action = string(applyActionAdopt)
			u.Detail = fmt.Sprintf("%s recorded %s here, and the server's state names %s — the server wins; this updates %s",
				wfdir.LockName, orNothing(r.From), r.To, r.To)
			continue
		}
		if unclaimed[addr] {
			u.Detail = strings.TrimPrefix(u.Detail+" — the server has no claim for it yet; apply claims it for this folder before writing", " — ")
		}
	}
}
