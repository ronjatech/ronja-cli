package commands

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The one comparison of a workflow, data-app or module folder with the row it
// is bound to that `sync apply --dry-run`, `sync status` and `sync adopt` all
// make: does live hold the folder (the push's own parity function), and, when
// asked, would the push's own overwrite check and title rule refuse it. Reads
// only, and through the push's functions, so neither can drift from the push.

// liveAsk is what compareLive is asked.
type liveAsk struct {
	// guard: when live does not hold the folder, ask the push's overwrite
	// check and title rule.
	guard bool
	// asPush: ask them as `sync apply`'s push does — of the row it writes to
	// (the open draft, a parentless draft, else a checkout of live, which
	// copies live), with the committed head anchor read and allowed to vouch.
	// False asks them of the live row alone with no anchor: adopt, which is
	// about to record that anchor, so it may not be its own witness.
	asPush bool
}

// liveCompared is what compareLive learned.
type liveCompared struct {
	// live: the row is live. neverPublished: it is a draft no publish has made
	// live.
	live, neverPublished bool
	// title is the bound row's.
	title string
	// held: live holds the folder exactly.
	held bool
	// checked: the overwrite check and title rule were asked, and refusal is
	// their answer, nil when the push would go on.
	checked bool
	refusal error
	// draft is the open draft they were asked of, nil when none was.
	draft *openDraft
}

// openDraft is an open draft of a bound row, as a draft_open refusal names it.
type openDraft struct {
	ID        string    `json:"id"`
	UpdatedAt time.Time `json:"updatedAt"`
	// DrafterUserID and UpdatedBy are a module draft's drafter and last
	// editor: a module has one draft, which may be a colleague's. Empty for a
	// workflow or data-app draft, which is always the caller's own.
	DrafterUserID string `json:"drafterUserID,omitempty"`
	UpdatedBy     string `json:"updatedBy,omitempty"`
}

// draftOpen reports that a comparison's refusal is the push's draft_open one.
func (c liveCompared) draftOpen() bool {
	var r draftOpenRefusal
	return c.draft != nil && errors.As(c.refusal, &r)
}

// compareLive reads the row f is bound to and compares it with local, the
// folder's files. A folder bound to nothing, and every other kind, answer the
// zero value.
func compareLive(ctx context.Context, f *folder, local map[string]string, ask liveAsk) (liveCompared, error) {
	client := f.client()
	switch f.Kind.Name {
	case wfdir.KindWorkflow:
		if f.Binding.WorkflowID != "" {
			return compareWorkflowLive(ctx, f, client, local, ask)
		}
	case wfdir.KindDataApp:
		if f.Binding.DataAppID != "" {
			return compareAppLive(ctx, f, client, local, ask)
		}
	case wfdir.KindModule:
		if f.Binding.ModuleID != "" {
			return compareModuleLive(ctx, f, client, local, ask)
		}
	}
	return liveCompared{}, nil
}

func compareWorkflowLive(ctx context.Context, f *folder, client *api.Client, local map[string]string, ask liveAsk) (liveCompared, error) {
	row, err := client.GetWorkflow(ctx, f.Binding.WorkflowID)
	if err != nil {
		return liveCompared{}, err
	}
	c := liveCompared{live: row.Lifecycle == api.LifecycleLive, neverPublished: row.Lifecycle == api.LifecycleDraft, title: row.Title}
	// The push refuses these before it compares a file.
	if ask.guard && ask.asPush {
		if refusal := cmp.Or(checkRuntimeDrift(f, row), checkLanguageDrift(f, row)); refusal != nil {
			c.checked, c.refusal = true, refusal
			return c, nil
		}
	}
	target, anchored := row, true
	var files []api.WorkflowFile
	switch {
	case c.live:
		if files, err = client.ListWorkflowFiles(ctx, row.ID); err != nil {
			return liveCompared{}, err
		}
		if c.held = workflowRowHoldsFolder(f, row, contentByPath(f.Codec, files), local); c.held || !ask.guard {
			return c, nil
		}
		if ask.asPush {
			draft, err := client.GetWorkflowDraft(ctx, row.ID)
			if err != nil {
				return liveCompared{}, err
			}
			if draft != nil {
				target, anchored = draft, draft.ParentWorkflowID == ""
				c.draft = &openDraft{ID: draft.ID, UpdatedAt: draft.UpdatedAt}
			}
		}
	case c.neverPublished && ask.guard && ask.asPush:
	default:
		return c, nil
	}
	if files == nil || target != row {
		if files, err = client.ListWorkflowFiles(ctx, target.ID); err != nil {
			return liveCompared{}, err
		}
	}
	head := headAgreement{}
	if ask.asPush {
		if head, err = askHeadAgreement(ctx, workflowHeadReader(client), f, row.ID, anchored); err != nil {
			return liveCompared{}, fmt.Errorf("could not read the version history of %s: %w", row.ID, err)
		}
	}
	c.checked = true
	_, c.refusal = checkDrift(f, target, contentByPath(f.Codec, files), hashFiles(f.baselineCodec(), files), local, false, head,
		driftAsk{draft: c.draft != nil, apply: true})
	c.refusal = beyondDraftOpen(f, row, c.refusal)
	if c.refusal == nil {
		// Never vouched: see the workflow title leg in runPush.
		c.refusal = titleRefusal(f, "workflow", &titledRow{ID: row.ID, Title: row.Title, RetitledInPlace: true},
			titledRow{ID: target.ID, Title: target.Title, Live: target == row && c.live}, false)
	}
	return c, nil
}

func compareAppLive(ctx context.Context, f *folder, client *api.Client, local map[string]string, ask liveAsk) (liveCompared, error) {
	row, err := client.GetDataApp(ctx, f.Binding.DataAppID)
	if err != nil {
		return liveCompared{}, err
	}
	c := liveCompared{live: row.Lifecycle == api.LifecycleLive, neverPublished: row.Lifecycle == api.LifecycleDraft, title: row.Name}
	// The push refuses this before it compares a file.
	if ask.guard && ask.asPush {
		if refusal := appEntrypointRefusal(f, row); refusal != nil {
			c.checked, c.refusal = true, refusal
			return c, nil
		}
	}
	target, anchored := row, true
	// The push's title leg is asked of the draft it writes to; a checkout of
	// live has no id yet, and is never live's.
	targetID := ""
	var files []api.DataAppFile
	switch {
	case c.live:
		if files, err = client.ListDataAppFiles(ctx, row.ID); err != nil {
			return liveCompared{}, err
		}
		if c.held = appRowHoldsFolder(f, row, appContentByPath(f.Codec, files), local); c.held || !ask.guard {
			return c, nil
		}
		if ask.asPush {
			draft, err := client.GetDataAppDraft(ctx, row.ID)
			if err != nil {
				return liveCompared{}, err
			}
			if draft != nil {
				target, anchored, targetID = draft, draft.ParentDataAppID == "", draft.ID
				c.draft = &openDraft{ID: draft.ID, UpdatedAt: draft.UpdatedAt}
			}
		}
	case c.neverPublished && ask.guard && ask.asPush:
		targetID = row.ID
	default:
		return c, nil
	}
	if files == nil || target != row {
		if files, err = client.ListDataAppFiles(ctx, target.ID); err != nil {
			return liveCompared{}, err
		}
	}
	head, vouches := headAgreement{}, false
	if ask.asPush {
		if head, err = askHeadAgreement(ctx, dataAppHeadReader(client), f, row.ID, anchored); err != nil {
			return liveCompared{}, fmt.Errorf("could not read the version history of %s: %w", row.ID, err)
		}
		vouches = appTitleHeadVouches(row, targetID, head)
	}
	c.checked = true
	_, c.refusal = checkAppDrift(f, appContentByPath(f.Codec, files), hashAppFiles(f.baselineCodec(), files), local, appMetaAgrees(f, target), false, head, target.ID,
		driftAsk{draft: c.draft != nil, apply: true})
	c.refusal = beyondDraftOpen(f, row, c.refusal)
	if c.refusal == nil {
		c.refusal = titleRefusal(f, "data app", &titledRow{ID: row.ID, Title: row.Name},
			titledRow{ID: target.ID, Title: target.Name, Live: target == row && c.live}, vouches)
	}
	return c, nil
}

func compareModuleLive(ctx context.Context, f *folder, client *api.Client, local map[string]string, ask liveAsk) (liveCompared, error) {
	row, err := client.GetModule(ctx, f.Binding.ModuleID)
	if err != nil {
		return liveCompared{}, err
	}
	c := liveCompared{live: row.Lifecycle == api.LifecycleLive, neverPublished: row.Lifecycle == api.LifecycleDraft, title: row.Title}
	if ask.guard && ask.asPush {
		if refusal := unpublishedModuleRenameError(f, row); refusal != nil {
			c.checked, c.refusal = true, refusal
			return c, nil
		}
	}
	target, anchored := row, true
	var files []api.ModuleFile
	switch {
	case c.live:
		if files, err = client.ListModuleFiles(ctx, row.ID); err != nil {
			return liveCompared{}, err
		}
		if c.held = moduleRowHoldsFolder(f, row, moduleContentByPath(files), local); c.held || !ask.guard {
			return c, nil
		}
		if ask.asPush {
			draft, err := client.GetModuleDraft(ctx, row.ID)
			if err != nil {
				return liveCompared{}, err
			}
			if draft != nil {
				target, anchored = draft, draft.ParentModuleID == ""
				c.draft = &openDraft{ID: draft.ID, UpdatedAt: draft.UpdatedAt, DrafterUserID: draft.DrafterUserID, UpdatedBy: draft.UpdatedBy}
			}
		}
	case moduleIsUnpublished(row) && ask.guard && ask.asPush:
	default:
		return c, nil
	}
	if files == nil || target != row {
		if files, err = client.ListModuleFiles(ctx, target.ID); err != nil {
			return liveCompared{}, err
		}
	}
	head := headAgreement{}
	if ask.asPush {
		if head, err = askHeadAgreement(ctx, moduleHeadReader(client), f, row.IdentityID(), anchored); err != nil {
			return liveCompared{}, fmt.Errorf("could not read the version history of %s: %w", row.IdentityID(), err)
		}
	}
	c.checked = true
	_, c.refusal = checkModuleDrift(f, row, moduleContentByPath(files), local, false, head, driftAsk{draft: c.draft != nil, apply: true})
	c.refusal = beyondDraftOpen(f, row, c.refusal)
	if c.refusal == nil {
		var live *titledRow
		if row.ID != target.ID {
			live = &titledRow{ID: row.ID, Title: row.Title, RetitledInPlace: true}
		}
		// Never vouched: see the module title leg in runModulePush.
		c.refusal = titleRefusal(f, "module", live, titledRow{ID: target.ID, Title: target.Title, Live: target == row && c.live}, false)
	}
	return c, nil
}

// liveHoldsFolder reports whether the LIVE row a workflow, data-app or module
// folder is bound to already holds the folder exactly: the comparison `sync
// apply` makes before anything else (pushNothingLiveHolds and its twins), by
// the same parity functions, so true means an apply pushes and publishes
// nothing for the folder. Reads only. False, with no error, for a folder bound
// to nothing, a row that is not live (a parentless draft awaits its publish),
// and every other kind.
func liveHoldsFolder(ctx context.Context, f *folder) (bool, error) {
	c, err := compareWithLive(ctx, f, false)
	return c.held, err
}

// compareWithLive is liveHoldsFolder, and with guard the push's own overwrite
// check and title rule asked as `sync apply`'s push asks them, so a dry run
// refuses exactly what the apply would.
//
// Under `ronja propose` (deployedAsCopy) a folder with a local baseline is
// asked twice: as itself, since a deploy of the copy would overwrite what this
// checkout knows changed on live since its last sync (a recorded version can
// vouch past it), and its refusal names the files; then as the copy the deploy
// runs on, which has no local baseline and so refuses what this checkout's
// push would not.
func compareWithLive(ctx context.Context, f *folder, guard bool) (liveCompared, error) {
	local, _, err := readLocalFiles(f.Root, f.Kind)
	if err != nil {
		return liveCompared{}, err
	}
	ask := liveAsk{guard: guard, asPush: true}
	c, err := compareLive(ctx, f, local, ask)
	if err != nil || c.held || !guard || c.refusal != nil || !deployedAsCopy(ctx) || f.State.For(f.Baseline()) == nil {
		return c, err
	}
	return compareLive(ctx, f.withoutLocalBaseline(), local, ask)
}

// withoutLocalBaseline is f as a copy of its files would open: no .ronja/,
// so no local baseline and no recorded proposals.
func (f *folder) withoutLocalBaseline() *folder {
	g := *f
	g.State = &wfdir.State{}
	return &g
}
