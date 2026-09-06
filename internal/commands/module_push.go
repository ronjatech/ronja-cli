package commands

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja module push` syncs the folder into the module's draft — never into the
// live module, which the server refuses file writes on anyway.
//
// The order of operations is the workflow loop's, and it is the design: local
// guards first (they need no network and their diagnosis is entirely local),
// then the drift guard (so an edit made elsewhere in the same draft is not
// silently overwritten), and only then the writes. Everything that can refuse
// does so before the first byte is persisted.
//
// The one structural difference from `wf push` is the missing validate step,
// and it is missing because there is nothing to validate remotely: a module
// carries no markers, so `module validate` is a local pass and running it as a
// pre-flight would tell the author only what these same local guards already
// refuse. See module_validate.go.
func newModulePushCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Sync this folder into the module's draft",
		Long: `Sync this folder into the module's draft.

Pushes to a DRAFT, always. If the module is live and there is no open draft, one
is checked out; if the folder has never been pushed to this instance, the module
is created, the binding recorded, and a draft checked out of it — a module in a
private feature is created LIVE, and live rows are immutable.

⚠️ A module has ONE draft, not one per author. A draft is a snapshot of the
whole file set, so two would each silently drop the other's edits at commit —
which means the draft your push writes to may be a colleague's. Only its drafter
and an admin may write to it, so an ordinary push into somebody else's draft is
refused outright; the per-file compare-and-swap is what protects the rest, by
refusing a file that changed since your last sync instead of overwriting it.

Push refuses when the draft changed on the server since your last sync, listing
what moved. --force overwrites.

In a SHARED feature a brand-new module is created as a PROPOSAL an admin must
approve, because a module edit fans out to every workflow that imports it.

Publishing is a separate step:

  ronja module publish             commit the draft, or submit it for review`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.ModuleKind)
			if err != nil {
				return err
			}
			result, err := runModulePush(cmd.Context(), f, modulePushOptions{Force: force})
			// The report is emitted even on failure: a push that stopped halfway
			// has already written files, and "which ones" is the first thing
			// anyone needs to know.
			if result != nil {
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printModulePushReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"push even though the draft changed on the server since your last sync")
	return cmd
}

type modulePushOptions struct {
	Force bool
}

// modulePushResult is the --json shape and the human renderer's input, so the
// two cannot describe different things.
type modulePushResult struct {
	// Created reports that this push brought the module into existence.
	Created bool `json:"created"`
	// Proposed reports that the module this push CREATED landed as a proposal
	// rather than as a draft — a brand-new module in a shared feature, which an
	// admin must approve before it exists for anyone else.
	//
	// A field of its own rather than a substring of the lifecycle, because it is
	// the one outcome of a first push that changes what the author has to do
	// next: `module publish` submits it, and until somebody approves it there is
	// no module for a workflow to import.
	Proposed bool `json:"proposed,omitempty"`
	// ModuleID is the STABLE identity (what ronja.json / ronja.lock.json
	// records); DraftID is the row the files were actually written to. For a
	// parentless draft or a proposal they are the same id.
	ModuleID  string   `json:"moduleID"`
	DraftID   string   `json:"draftID"`
	Pushed    []string `json:"pushed"`
	Deleted   []string `json:"deleted"`
	Unchanged int      `json:"unchanged"`
	// UpToDate reports a push that had nothing to do: the folder, the draft and
	// the baseline already agree, so not one byte was written.
	UpToDate bool `json:"upToDate"`
	// DraftUnderReview reports that the draft written to has already been
	// submitted for admin review — the push changed what someone is reviewing.
	DraftUnderReview bool `json:"draftUnderReview"`
	// Metadata names the metadata this push actually CHANGED on the live row
	// (the package name, the title). Empty when the manifest and the row already
	// agreed, and empty for an unpublished module, whose metadata cannot be
	// patched at all — see MetadataDeferred.
	Metadata string `json:"metadata,omitempty"`
	// MetadataDeferred reports a manifest whose name or title differs from an
	// UNPUBLISHED module's, which no request can reconcile: PUT /module/:id
	// takes the live row only. It is reported rather than silently skipped
	// because the folder and the server genuinely disagree, and the difference
	// survives until the module is published.
	MetadataDeferred string `json:"metadataDeferred,omitempty"`
	// Error describes a push that stopped part-way. The result above then
	// describes what DID happen before it stopped, which is the state the draft
	// is now in.
	Error string `json:"error,omitempty"`
	// Conflict reports that what stopped this push was a file precondition
	// refusing — somebody wrote to the draft between the listing this push
	// compared against and its own write.
	//
	// It exists because Error is PROSE, and a caller that has to tell "somebody
	// got there first, re-read and re-apply" from "this file was rejected" would
	// otherwise have only substring-matching to do it with.
	Conflict bool `json:"conflict"`
	// Target names the instance and organization this push landed in, so a
	// mistake is visible where it happens rather than only from a later status.
	Target string `json:"target,omitempty"`
}

// runModulePush is the whole state machine, kept out of the cobra closure so it
// is testable as a function and so the reporting path has exactly one shape.
//
// It returns (result, err): a non-nil result with a non-nil error is the
// partial-push case, and both halves matter.
func runModulePush(ctx context.Context, f *folder, opts modulePushOptions) (*modulePushResult, error) {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	result := &modulePushResult{
		ModuleID: f.Binding.ModuleID,
		Target:   describeTarget(f.Resolved),
		Pushed:   []string{},
		Deleted:  []string{},
	}

	// 1. Local guards. Nothing here needs the network, and each refusal names a
	// file the author can act on immediately.
	local, enumeration, err := readLocalFiles(f.Root, wfdir.ModuleKind)
	if err != nil {
		return nil, err
	}
	noteModuleSkipped(enumeration)
	if err := checkPushable(local, wfdir.ModuleKind, maxModuleFileBytes); err != nil {
		return nil, err
	}
	// The package's own __init__.py, refused HERE rather than mid-push. Without
	// it the reconcile below would try to DELETE the server's seeded copy, which
	// the server refuses — so the push would create the module and then fail on
	// a deletion the author never asked for, leaving a row nobody meant to make.
	if _, ok := local[moduleInitFile]; !ok {
		return nil, fmt.Errorf("this folder has no %s, and a module is a Python package — `import %s` would not resolve without one.\n  Create it (an empty file is fine); the server refuses to delete it, so a push cannot reconcile a folder that is missing it",
			moduleInitFile, moduleName(f))
	}
	// The ONE content rule a module save has, refused HERE rather than mid-push.
	// The server refuses a marker one file at a time, so without this the push
	// creates the module (or writes into the draft), gets as far as the offending
	// file and stops — leaving a half-written draft to explain a rule the folder
	// broke before the first request. It is the same lexical scan `module
	// validate` reports (module_markers.go), stopping at the first file because
	// a push refuses rather than reports.
	if err := refuseModuleMarkers(local); err != nil {
		return nil, err
	}

	// 2. Read what is already on the server for this binding. Pure reads, and
	// deliberately before anything is written.
	existing, err := inspectModuleTarget(ctx, client, f)
	if err != nil {
		return nil, err
	}
	featureID, err := moduleFeatureIDFor(f, existing.Module)
	if err != nil {
		return nil, err
	}

	// 3. Resolve the row to write to — the first step that changes anything.
	target, createdRow, err := resolveModulePushTarget(ctx, client, f, featureID, existing, result)
	if err != nil {
		return nil, err
	}
	result.DraftID = target.ID
	// The IDENTITY row — the one that actually holds this module's name and
	// title, and the only one PUT /module/:id will patch. It is `target` only
	// while the module is unpublished, where the row IS the module.
	//
	// The DRIFT GUARD and the BASELINE read it rather than `target`, and that is
	// the fix for a real bug rather than tidiness: a draft carries a COPY of the
	// parent's name and title taken at checkout. Recording the draft's values in
	// the baseline made an ordinary title edit read as "the draft changed on the
	// server" on every later push (a false refusal), while a colleague's rename
	// of the live row read as no drift at all and was silently pushed back (a
	// false pass). `module status` already compares the live row — see
	// moduleRemoteStatus.
	//
	// The draft's copy is not left stale, though: step 6b reconciles it, because
	// the commit publishes the DRAFT's metadata onto the live row and a stale
	// copy reverts the edit at the next publish. Two rows, two patches, one set
	// of intended values — and only the identity row's are compared or recorded.
	identity := moduleIdentityRow(existing, createdRow, target)
	if target.SubmittedForReviewAt != nil {
		result.DraftUnderReview = true
		fmt.Fprintf(os.Stderr, "  Warning: draft %s has already been submitted for review — this push changes what the admin is reviewing.\n",
			target.ID)
	}

	// 4. Read what the row holds. ALWAYS, including for a module this push just
	// created: creating a module SEEDS an __init__.py inside the same
	// transaction, so "just created" does not mean "empty". A push that skipped
	// this read and then failed on its own first PUT would record a baseline of
	// zero files against a row holding that seed — and the retry would read the
	// seed as somebody else's drift and demand --force for a module the CLI made
	// seconds earlier.
	files, err := client.ListModuleFiles(ctx, target.ID)
	if err != nil {
		return nil, fmt.Errorf("read files of %s: %w", target.ID, err)
	}
	remoteFiles := moduleContentByPath(files)

	// The drift GUARD is what a fresh create skips, and only that: nothing on
	// the server is older than this push, so there is nothing anyone could have
	// changed underneath it.
	baselineClean := false
	preconditions := filePreconditions{}
	head := headAgreement{}
	if !result.Created {
		head = readHeadAgreement(ctx, moduleHeadReader(client), f, existing.Module.IdentityID(),
			existing.AnchoredDraft())
		verdict, err := checkModuleDrift(f, identity, remoteFiles, opts.Force, head)
		if err != nil {
			return nil, err
		}
		baselineClean = verdict.BaselineClean
		// Server-side preconditions are armed EXACTLY where the drift guard
		// vouched for the baseline, and suppressed everywhere it was bypassed.
		// See filePreconditions for why that equivalence is the whole rule.
		if !verdict.Bypassed {
			preconditions = filePreconditions{Armed: true, Hashes: verdict.Preconditions}
		}
	}

	// landed tracks what the baseline may honestly claim as the sync proceeds,
	// so a push that stops half way can still record an accurate one. stop is
	// the single exit for every failure after this point: whatever went wrong,
	// the baseline has to describe what DID land. Leaving it stale would make
	// the author's own half-finished push read as someone else's drift on the
	// next attempt — refusing the retry that fixes it.
	//
	// It starts as what is ALREADY acknowledged rather than as the whole remote:
	// a --force push that stops must not hand the retry a baseline containing
	// the very content it was forcing past. Shared with the workflow loop
	// verbatim (acknowledgedRemote), because the rule is the same rule.
	landed := acknowledgedRemote(f.State.For(f.Key), remoteFiles, local, result.Created)
	stop := func(err error) (*modulePushResult, error) {
		result.Error = err.Error()
		saveModuleBaseline(f, target, identity, landed)
		return result, err
	}

	// 5. Writes first. __init__.py leads, for the reason the workflow loop leads
	// with the entrypoint: it is the file that makes the row a package at all,
	// so a half-finished sync still leaves something importable.
	if err := putModuleFiles(ctx, client, target, local, remoteFiles, landed, preconditions, result); err != nil {
		return stop(err)
	}

	// 6. Then the metadata — TWO rows, where the workflow loop patches one.
	//
	// `wf push` patches the draft and the commit publishes it onto the live row.
	// A module cannot do that in one request: PUT /module/:id refuses a draft id,
	// since a module's name is claimed against the live-uniqueness index and a
	// draft has no claim to make. So the live row is patched here (6a) and the
	// open draft is patched through its own route (6b) — because the commit
	// overwrites the parent's name and title with the DRAFT's copies, and a
	// draft still holding what it was checked out with reverts the edit the
	// moment somebody publishes.
	//
	// An UNPUBLISHED module has neither row: no live row to patch, and its own
	// row is a parentless draft that DraftEdit does not take either. The create
	// carried the manifest's values, and a later manifest edit reaches the row
	// only at the first publish — reported as deferred rather than silently
	// dropped.
	patch, deferred := moduleMetadataPatchFor(f, existing, target)
	result.MetadataDeferred = deferred
	if deferred != "" {
		fmt.Fprintf(os.Stderr, "  Note: %s\n", deferred)
	}
	// existing.Module is nil on the push that CREATED the module, and the
	// dereference below would panic on it. moduleMetadataPatchFor already
	// returns an empty patch in that case, so the guard is redundant TODAY —
	// which is exactly why it is stated: the safety of this line currently
	// lives in another function's first branch, where a later "also patch on
	// create" change would remove it without anything here looking wrong.
	if existing.Module != nil && !patch.Empty() {
		updated, err := client.UpdateModule(ctx, existing.Module.IdentityID(), patch)
		if err != nil {
			return stop(fmt.Errorf("update %s: %w", describeModulePatch(patch), err))
		}
		// The identity row holds this now, and it is what the baseline recorded
		// from here on describes — including the one stop() writes if the
		// deletions below fail. Leaving it stale would record metadata the server
		// stopped holding a request ago, and the next push would read its own
		// change back as somebody else's.
		//
		// ⚠️ Mirrored onto `identity` ONLY. This request landed on the live row,
		// and the draft's own copy of the name and title has not moved yet —
		// step 6b is what moves it, and it mirrors its own patch onto `target`
		// when the server accepts it. Mirroring here as well would claim a write
		// that has not happened, and the baseline records both rows.
		applyModulePatch(identity, patch)
		if updated != nil {
			identity.Name, identity.Title = updated.Name, updated.Title
		}
		result.Metadata = describeModulePatch(patch)
	}

	// 6b. The DRAFT's own copy of the metadata, which is what the commit
	// actually publishes. Computed against the draft rather than derived from
	// 6a's patch, and that independence is the point: the two rows can be out of
	// step for reasons this push did not cause — a rename made in the browser
	// and then written into the manifest leaves 6a with nothing to do and the
	// draft still stale — and a step that only ran when 6a did would leave that
	// draft to revert the name at the next publish.
	//
	// Empty for an unpublished module (no live parent, so no draft of one) and
	// for a draft that already agrees with the manifest, which is the ordinary
	// case: this is one extra round trip on the push that changes a title, and
	// none at all on the pushes after it.
	draftPatch := moduleDraftMetadataPatchFor(f, target)
	if !draftPatch.Empty() {
		// Addressed by the PARENT's id: DraftEdit is get-or-create-then-patch on
		// the live module, and the draft it resolves is the one `target` names.
		patched, err := client.CheckoutModule(ctx, target.ParentModuleID, draftPatch)
		if err != nil {
			return stop(fmt.Errorf("update %s on the open draft of %s: %w",
				describeModulePatch(draftPatch), target.ParentModuleID, err))
		}
		applyModulePatch(target, draftPatch)
		if patched != nil {
			// The row's updatedAt moved, and the baseline records it from
			// `target`. Taking it from the response rather than guessing keeps
			// the baseline describing a row state the server actually held.
			target.Name, target.Title, target.UpdatedAt = patched.Name, patched.Title, patched.UpdatedAt
		}
		if result.Metadata == "" {
			result.Metadata = describeModulePatch(draftPatch)
		}
	}

	// 7. Deletions last, so a rename-shaped change has already landed its new
	// file before the old one goes.
	if err := deleteModuleFiles(ctx, client, target, local, remoteFiles, landed, preconditions, result); err != nil {
		return stop(err)
	}

	// 8. A push that wrote nothing at all, against a baseline that already
	// described the server, has nothing to re-read. Both metadata patches count:
	// a push that only reconciled the draft's stale copy still wrote something.
	if !result.Created && baselineClean && len(result.Pushed) == 0 && len(result.Deleted) == 0 &&
		patch.Empty() && draftPatch.Empty() {
		result.UpToDate = true
		// It may still have something to RECORD: a push that had no files to
		// write can perfectly well have checked out a fresh draft on the way
		// here, and the baseline then names the row the files came from rather
		// than the one the next push will compare against. Written only when it
		// would actually change.
		if !moduleBaselineDescribes(f.State.For(f.Key), target, identity) {
			f.State.Set(f.Key, moduleBaselineFromLocal(target, identity, landed))
			if err := wfdir.SaveState(f.Root, f.State); err != nil {
				return result, err
			}
		}
		return result, anchorAfterPush(f, head, opts.Force)
	}

	// 9. The new baseline is what we just pushed: the server now holds exactly
	// these bytes on `target`, and `identity` carries the metadata as patched
	// above.
	f.State.Set(f.Key, moduleBaselineFromLocal(target, identity, local))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		return result, err
	}
	return result, anchorAfterPush(f, head, opts.Force)
}

// moduleName is the package name this folder means, for a message. The
// manifest's when it declares one, a placeholder otherwise — this is prose, not
// a value anything acts on.
func moduleName(f *folder) string {
	if f.Manifest.Name != "" {
		return f.Manifest.Name
	}
	return "<name>"
}

// existingModuleTarget is what the server already holds for a folder's binding:
// the row the binding names, plus the open draft of it when that row is live.
//
// It exists so the READS a push needs happen once, before anything is written,
// and the WRITES (create, checkout) stay behind it.
type existingModuleTarget struct {
	// Module is the row the binding names: nil for an unbound folder, and itself
	// a parentless draft or a proposal when the module has never been published.
	Module *api.Module
	// Draft is the open draft of a LIVE Module, nil when there is none (or when
	// Module is already a draft or proposal).
	Draft *api.Module
}

// Row returns the row a push would write to without creating anything, or nil
// when a create or a checkout is still needed.
func (e existingModuleTarget) Row() *api.Module {
	if e.Draft != nil {
		return e.Draft
	}
	if moduleIsUnpublished(e.Module) {
		return e.Module
	}
	return nil
}

// AnchoredDraft reports that the row a push writes to is one the committed head
// anchor can speak for — see headAgreement.AnchoredDraft, which documents the
// two qualifying shapes and the one they exclude.
//
// The parentless leg is not a relaxation of the guard, it is the guard applied
// to the right row: a module that has never been published has no live version
// to protect, so the row IS the module and an unversioned row's head is its own
// id. A DRAFT forked from a live module is excluded, because the anchor is a
// fact about the live row and says nothing about a half-finished draft.
func (e existingModuleTarget) AnchoredDraft() bool {
	row := e.Row()
	if row == nil {
		return true
	}
	return row.ParentModuleID == ""
}

// inspectModuleTarget reads the binding's current state without changing
// anything.
//
// Every refusal a push makes about the row it was pointed at lives here, so
// they all happen before the first write — including before the module a first
// push would create.
func inspectModuleTarget(ctx context.Context, client *api.Client, f *folder) (existingModuleTarget, error) {
	if f.Binding.ModuleID == "" {
		return existingModuleTarget{}, nil
	}
	mod, err := client.GetModule(ctx, f.Binding.ModuleID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			// ⚠️ Two causes, and the message covers both because the CLI cannot
			// tell them apart. GET /module/:id resolves live modules AND the
			// caller's own draft or proposal, so this is either a module that is
			// really gone (or that you lost access to), or somebody ELSE's
			// in-flight row — which answers 404 rather than 403 precisely so that
			// it discloses nothing.
			return existingModuleTarget{}, fmt.Errorf("cannot read module %s on %s.\n  Either it no longer exists (or you lost access to it), or it is an unpublished draft or proposal belonging to somebody else, which this route will not resolve for you.\n  `ronja module status` says which; to start over, remove the binding from %s",
				f.Binding.ModuleID, f.Resolved.URL, wfdir.ManifestPath(f.Root))
		}
		return existingModuleTarget{}, err
	}
	if err := refuseUnclonableModule(mod); err != nil {
		return existingModuleTarget{}, err
	}
	if moduleIsUnpublished(mod) {
		// The binding names the module itself — a parentless draft or a proposal.
		// Asking for a draft OF it is not a thing.
		return existingModuleTarget{Module: mod}, nil
	}
	draft, err := client.GetModuleDraft(ctx, mod.ID)
	if err != nil {
		return existingModuleTarget{}, fmt.Errorf("check for the open draft of %s: %w", mod.ID, err)
	}
	return existingModuleTarget{Module: mod, Draft: draft}, nil
}

// resolveModulePushTarget decides which row the files go to, creating the
// module on a first push and checking out a draft when there is none.
// Everything it could refuse has already been refused by inspectModuleTarget.
//
// Never auto-creates a REPLACEMENT: a binding that names a module which has
// been deleted or archived is reported as broken. The alternative — quietly
// creating a second module — would leave every workflow that imports the old
// one pointing at a row nobody edits any more, and would fail anyway on the
// live-name uniqueness index.
func resolveModulePushTarget(ctx context.Context, client *api.Client, f *folder, featureID string, existing existingModuleTarget, result *modulePushResult) (target, created *api.Module, err error) {
	if existing.Module == nil {
		if f.Manifest.Name == "" {
			return nil, nil, fmt.Errorf("%s declares no \"name\", and a module cannot be created without one — it is the identifier a workflow writes after `import`. Add it, or recreate the folder with `ronja module init <name> --feature <id>`",
				wfdir.ManifestPath(f.Root))
		}
		// A first push is the one path here that WRITES a binding, so this is
		// where the organization has to be authoritative rather than adopted from
		// an entry that does not exist yet. Only when this folder is NOT bound
		// here — see folder.bindNew.
		if !f.Bound {
			if _, err := f.bindNew(ctx); err != nil {
				return nil, nil, err
			}
		}
		created, err := client.CreateModule(ctx, api.CreateModuleInput{
			FeatureID: featureID,
			Name:      f.Manifest.Name,
			Title:     f.Manifest.Title,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("create module %q in feature %s: %w", f.Manifest.Name, featureID, err)
		}
		// Recorded IMMEDIATELY, before any file is written: the module now
		// exists, and a push that died before saving the manifest would leave an
		// orphan the next push could not find and would create again — where
		// "again" means a 409 on the package name, for ever.
		f.recordBinding(wfdir.Binding{ModuleID: created.ID, FeatureID: featureID})
		// A module that has just been created has never been versioned, and an
		// unversioned row IS its own head — the same fallback
		// api.ModuleHeadVersionID resolves and the same one rmodule's commit CAS
		// resolves. Recorded here rather than by a round trip, in the write that
		// records the binding it describes: they are one fact, and a folder
		// holding half of it is a folder with an anchor pointing at nothing.
		f.setHeadVersion(created.ID)
		if err := f.saveFolder(); err != nil {
			return nil, nil, fmt.Errorf("record the new module %s in %s: %w",
				created.ID, f.bindingFiles(), err)
		}
		result.Created = true
		result.ModuleID = created.ID
		// Read off the CREATED row, never off the row returned below: a private
		// create comes back live and gets a checkout, and asking the draft
		// whether it is a proposal would answer "no" for a shared feature too.
		result.Proposed = created.Lifecycle == api.LifecycleProposed
		// ⚠️ A private-feature create is born LIVE. rmodule.Add stamps the drafter
		// fields only on the shared branch, so the lifecycle trigger classifies a
		// private module as 'live' and requireDraft refuses the very first file
		// write into it ("file mutations are only allowed on draft or proposed
		// modules"). A SHARED create comes back 'proposed', which IS writable, so
		// it must not be checked out — checkout takes a live parent and would be
		// refused. moduleIsUnpublished is exactly that question.
		//
		// The checkout runs AFTER the binding and the anchor are recorded, and
		// that order is load-bearing for the reason stated above: the module
		// exists from the create onwards, and a push that died before saving the
		// manifest would leave an orphan the next push could not find.
		if moduleIsUnpublished(created) {
			return created, created, nil
		}
		// Empty patch, for the reason the existing-module branch below gives —
		// and here there is nothing to apply anyway: the create body carried the
		// manifest's name and title, and the draft copies them from the row.
		draft, err := client.CheckoutModule(ctx, created.ID, api.ModulePatch{})
		if err != nil {
			return nil, nil, fmt.Errorf("check out a draft of the module just created (%s): %w", created.ID, err)
		}
		return draft, created, nil
	}

	if row := existing.Row(); row != nil {
		return row, nil, nil
	}
	// An EMPTY patch, deliberately: this checkout runs BEFORE the drift guard,
	// and seeding the draft with a manifest title nothing has compared against
	// the live row yet would apply an edit the guard is about to refuse. The
	// draft's metadata is reconciled in step 6b, after the guard.
	draft, err := client.CheckoutModule(ctx, existing.Module.ID, api.ModulePatch{})
	if err != nil {
		return nil, nil, fmt.Errorf("check out a draft of %s: %w", existing.Module.ID, err)
	}
	return draft, nil, nil
}

// checkModuleDrift refuses a push that would overwrite server-side changes made
// since the last sync.
//
// It is `wf push`'s checkDrift with the workflow-only metadata legs replaced by
// the module's two (name, title) and shares its verdict type verbatim, because
// the three layers are the same three layers: a baseline comparison at one
// instant, per-file preconditions closing the window after it, and the
// committed head anchor for a checkout that has no baseline at all.
//
// The comparison is REMOTE vs BASELINE, not remote vs local: the question is
// whether anything moved underneath us, and a file the author edited locally is
// exactly what a push is for.
//
// The two halves read DIFFERENT rows, and that is the point rather than an
// inconsistency: the files come from the row the push writes to (the draft),
// and `identity` is the row whose metadata a push would change (the live one,
// or the module itself while it is unpublished). See moduleIdentityRow.
func checkModuleDrift(f *folder, identity *api.Module, remote map[string]string, force bool, head headAgreement) (driftVerdict, error) {
	baseline := f.State.For(f.Key)
	remoteHashes := make(map[string]string, len(remote))
	for path, content := range remote {
		remoteHashes[path] = wfdir.HashString(content)
	}
	drift := wfdir.DiffHashes(remoteHashes, baseline.Hashes())
	meta := moduleMetadataDriftOf(baseline, identity, f.Manifest)
	if !drift.Dirty() && len(meta) == 0 {
		// Bypassed still tracks --force here, even though nothing needed
		// bypassing: it is what suppresses the per-file preconditions, and
		// --force means "overwrite the remote with what I have" whether or not
		// the baseline happened to be clean when this check ran.
		return driftVerdict{BaselineClean: baseline != nil, Bypassed: force, Preconditions: baseline.Hashes()}, nil
	}
	if force {
		fmt.Fprintf(os.Stderr, "  Note: --force — overwriting %s that changed on the server since your last sync.\n",
			describeDrifted(drift, meta))
		return driftVerdict{Bypassed: true}, nil
	}
	if baseline == nil {
		// A first push that died between recording the binding and writing the
		// baseline leaves exactly this: bound, no baseline, and a row holding
		// nothing but the __init__.py the CREATE seeded. Refusing it would be
		// refusing the CLI's own half-finished work with a message about somebody
		// else's — so it proceeds as the first push it is.
		//
		// Deliberately narrow: an unpublished module (never committed, so nobody
		// else has had a reason to touch it) holding ONE file, at __init__.py. A
		// colleague's folder from git names a live module, or a draft with a file
		// set someone actually built, and still refuses.
		// `identity` and the row a push writes to are the SAME row here: an
		// unpublished module has no live row behind it, which is what
		// isUntouchedFirstModulePush is asking about.
		if isUntouchedFirstModulePush(identity, remote) {
			return driftVerdict{Bypassed: true}, nil
		}
		// THE FRESH-CHECKOUT PATH. `.ronja/` is git-ignored, so this is what CI
		// and every colleague's `git clone` looks like, and without the committed
		// anchor the only way through would be --force, which ALSO disarms the
		// per-file preconditions.
		if head.Vouches() {
			noteHeadAnchored(identity.ID, head.Current)
			// Armed on the REMOTE listing rather than bypassed: the anchor
			// established that those bytes are the live row's, unchanged since
			// this folder forked from it, so asserting them is asserting agreement
			// rather than asserting a guess.
			return driftVerdict{HeadVouched: true, Preconditions: remoteHashes}, nil
		}
		if head.Moved() {
			return driftVerdict{}, refuseHeadMoved("module", f.Binding.ModuleID, head,
				"`ronja module clone "+f.Binding.ModuleID+"`")
		}
		// The anchor agreed about the LIVE row and still could not vouch, which
		// leaves exactly one cause: a draft over the live row was already open
		// when this push started. It may hold an edit nothing here has ever seen.
		//
		// ⚠️ The remedy names `module discard`, and unlike the workflow case that
		// draft may be a COLLEAGUE'S — a module has one draft, not one per author
		// — so the wording says whose work is at stake rather than assuming it is
		// the reader's.
		if head.Recorded != "" && head.Recorded == head.Current {
			return driftVerdict{}, fmt.Errorf("this folder has no sync baseline for %s, and module %s already has an open draft there — %s records which live version this folder forked from, but nothing here can say what is in that draft.\n  A module has ONE draft, so it may be a colleague's: look at it first. `ronja module discard` throws it away and pushes onto the live version, and push --force overwrites it",
				f.Resolved.URL, f.Binding.ModuleID, wfdir.LockName)
		}
		return driftVerdict{}, fmt.Errorf("this folder has no sync baseline for %s, and the module already has %d file(s) there — .ronja/ is local-only, so a copy cloned from git starts without one.\n  Clone the module into a fresh folder to get one, or push --force to overwrite the remote files with what you have here",
			f.Resolved.URL, len(remote))
	}
	return driftVerdict{}, fmt.Errorf("the draft changed on the server since your last sync — pushing would overwrite it:\n%s  Run `ronja module status` to see the detail, or push --force to overwrite",
		driftSummary(drift, meta))
}

// moduleMetadataDriftOf reports the metadata this push would overwrite.
//
// Same test as a file, in both halves: the remote has to differ from what the
// last sync recorded (something moved underneath us) AND from what the folder
// wants (we would overwrite it).
//
// `identity` is the row a push would PATCH — the live module, or the module
// itself while it is unpublished (moduleIdentityRow) — never the draft the files
// go to. A draft's name and title are a copy taken at checkout that no patch
// updates, so comparing them would report the reader's own last title edit as a
// colleague's change, and miss the colleague's actual rename.
//
// A baseline with no metadata recorded at all is a state file written before
// this existed. It reports NO drift rather than guessing: turning every folder
// that predates the guard into a refusal would be a strange way to start
// protecting them, and the first push to record a baseline arms it.
func moduleMetadataDriftOf(baseline *wfdir.InstanceState, identity *api.Module, m *wfdir.Manifest) []metadataDrift {
	if baseline == nil {
		return nil
	}
	var out []metadataDrift
	if m.Title != "" && baseline.Title != "" && identity.Title != baseline.Title {
		out = append(out, metadataDrift{Field: "title", Baseline: baseline.Title, Remote: identity.Title})
	}
	if m.Name != "" && baseline.Name != "" && identity.Name != baseline.Name {
		out = append(out, metadataDrift{Field: "package name", Baseline: baseline.Name, Remote: identity.Name})
	}
	return out
}

// moduleMetadataPatchFor works out what a push should patch, and what it
// CANNOT.
//
// The patch is computed against the LIVE row rather than the draft, because
// that is the row PUT /module/:id writes: a draft inherits the module's identity
// rather than carrying its own claim on the package name. When the module is
// still unpublished there is no live row at all, so a manifest that has moved on
// since the create is reported as deferred rather than sent to a route that
// would refuse it.
func moduleMetadataPatchFor(f *folder, existing existingModuleTarget, target *api.Module) (api.ModulePatch, string) {
	if existing.Module == nil {
		// A module this push just created already carries the manifest's values —
		// they rode the create body — so there is nothing to patch and nothing to
		// defer.
		return api.ModulePatch{}, ""
	}
	if moduleIsUnpublished(existing.Module) {
		pending := moduleMetadataPatch(f.Manifest, target)
		if pending.Empty() {
			return api.ModulePatch{}, ""
		}
		return api.ModulePatch{}, fmt.Sprintf(
			"%s is not published yet, so %s cannot be changed on it — a module's metadata is patched on the LIVE row, and this one has none. It is applied by `ronja module publish`",
			existing.Module.ID, describeModulePatch(pending))
	}
	return moduleMetadataPatch(f.Manifest, existing.Module), ""
}

// moduleDraftMetadataPatchFor is the metadata a push must ALSO write onto the
// open draft, empty when there is nothing to do.
//
// It exists because a commit is not a merge: rmodule's commitDraftInTx
// overwrites the parent's name, title and description with the DRAFT's copies —
// the copies taken when the draft was checked out. So a title patched onto the
// live row alone survives exactly until somebody publishes, and then reverts,
// silently, to a value nobody typed. `wf push` never meets this because it
// patches the row it is going to commit; the module loop patches the live row
// because PUT /module/:id refuses a draft id, so it has to reach the draft too.
//
// Empty for a row with no live parent — a parentless draft or a proposal IS the
// module, there is no second row holding a copy, and DraftEdit refuses a
// non-live id anyway.
func moduleDraftMetadataPatchFor(f *folder, target *api.Module) api.ModulePatch {
	if target == nil || target.ParentModuleID == "" {
		return api.ModulePatch{}
	}
	return moduleMetadataPatch(f.Manifest, target)
}

// isUntouchedFirstModulePush reports the row a crashed first push leaves
// behind: a never-committed module holding nothing but the __init__.py the
// server seeded when it created it.
func isUntouchedFirstModulePush(target *api.Module, remote map[string]string) bool {
	if !moduleIsUnpublished(target) {
		return false
	}
	if len(remote) != 1 {
		return false
	}
	_, seeded := remote[moduleInitFile]
	return seeded
}

// moduleIdentityRow is the row that HOLDS a module's name and title: the live
// module, or the module itself while it has never been published (a parentless
// draft or a proposal, where the row IS the module).
//
// It is not the row a push writes files to, and the difference is exactly what
// the metadata guard turns on. `PUT /module/:id` takes the live row — a module's
// name is claimed against the live-uniqueness index and a draft has no claim to
// make — while a draft carries a COPY of the name and title taken at checkout,
// which no patch ever updates. Compare or record the draft's copy and an
// ordinary title edit reads as somebody else's drift on the next push, while a
// colleague's real rename reads as no drift at all.
// `created` is the row a first push's CREATE returned, nil when this push
// created nothing. It is a third argument rather than "whatever target happens
// to be" because a private-feature create comes back LIVE and is immediately
// checked out, so on that path `target` is a DRAFT OF the module and the module
// itself is only reachable through this parameter.
func moduleIdentityRow(existing existingModuleTarget, created, target *api.Module) *api.Module {
	if existing.Module != nil {
		return existing.Module
	}
	if created != nil {
		return created
	}
	// Nothing was created and the binding named nothing: `target` is all there
	// is. Unreachable through runModulePush today, and the honest answer anyway.
	return target
}

// moduleBaselineDescribes reports whether the baseline already names the row a
// push just resolved AND the metadata the identity row holds — the test that
// keeps a no-op push from re-stamping a baseline and moving its timestamp to
// describe a sync that never happened.
//
// Two rows, for the reason moduleIdentityRow gives: `source` is where the files
// came from, `identity` is where the name and title live.
func moduleBaselineDescribes(baseline *wfdir.InstanceState, source, identity *api.Module) bool {
	if baseline == nil {
		return false
	}
	return baseline.SourceID == source.ID &&
		baseline.Title == identity.Title &&
		baseline.Name == identity.Name
}

// saveModuleBaseline records what a stopping push may honestly claim. It runs
// on the partial-push path, where the push has already failed and a second
// failure must not replace the first in the report.
func saveModuleBaseline(f *folder, source, identity *api.Module, files map[string]string) {
	f.State.Set(f.Key, moduleBaselineFromLocal(source, identity, files))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not record what was pushed in the local baseline (%v).\n", err)
	}
}

// putModuleFiles writes the folder's files into the target row, __init__.py
// leading and the rest in path order so a push is reproducible
// request-for-request.
//
// Failure STOPS rather than continuing with the rest. A push is already
// non-atomic across files, and carrying on after a rejection would spread the
// damage while making the report harder to read; the caller reports what landed
// before the stop, and re-pushing heals.
//
// The two maps are different questions. `remote` is what the server held when
// this push started, and it DECIDES: a file whose content is already there is
// skipped. `landed` is what the baseline may claim, and it RECORDS.
func putModuleFiles(ctx context.Context, client *api.Client, target *api.Module, local, remote, landed map[string]string, pre filePreconditions, result *modulePushResult) error {
	targetID := target.ID
	order := []string{}
	if _, ok := local[moduleInitFile]; ok {
		order = append(order, moduleInitFile)
	}
	for _, path := range sortedPaths(local) {
		if path != moduleInitFile {
			order = append(order, path)
		}
	}

	for _, path := range order {
		if remoteContent, ok := remote[path]; ok && remoteContent == local[path] {
			result.Unchanged++
			continue
		}
		if _, err := client.PutModuleFile(ctx, targetID, path, local[path], pre.ForWrite(path)); err != nil {
			// A 409 is the precondition refusing: the file moved between the
			// listing this push compared against and this write. Nothing landed,
			// nothing is reconciled, and the author is told what to do — never
			// retried and never escalated to --force automatically.
			if api.StatusOf(err) == api.StatusConflict {
				result.Conflict = true
				noteFileConflict("module", "module", "overwritten", path)
				return fmt.Errorf("push %s: %w", path, err)
			}
			if api.StatusOf(err) == 404 {
				return moduleFileWriteRefusal("push", path, target, err)
			}
			// A PUT that TIMED OUT may still have committed: the deadline was
			// ours, the transaction was the server's. A rejection is different —
			// the server considered the write and refused it, and the file
			// certainly kept its previous content. rmodule.UpsertFileChecked does
			// all of its work inside ONE transaction under the module's row lock,
			// so the workflow loop's narrow timeout-only trigger is right here
			// too: widening it would mean asking after refusals that settled
			// themselves, and asking is precisely how a colleague's content gets
			// read back into a baseline.
			if api.IsTimeout(err) && reconcileTimedOutModulePut(ctx, client, targetID, path, local[path], landed) {
				// It landed after all, so the report has to say so — a report that
				// disagrees with the baseline written beside it is worse than
				// either of them being wrong on its own.
				result.Pushed = append(result.Pushed, path)
			}
			return fmt.Errorf("push %s: %w", path, err)
		}
		landed[path] = local[path]
		result.Pushed = append(result.Pushed, path)
	}
	return nil
}

// reconcileTimedOutModulePut asks what the server actually holds for one file
// after a write whose outcome is unknown, and acknowledges it only when the
// answer is what the write meant to leave behind.
//
// One read, no retry loop: the question has a single answer and it is worth
// exactly one round trip. A read that ALSO fails changes nothing — the file
// keeps whatever the baseline already acknowledged.
//
// Reports whether the write is now known to have landed, so the push's report
// and the baseline written beside it describe the same set of files.
func reconcileTimedOutModulePut(ctx context.Context, client *api.Client, targetID, path, want string, landed map[string]string) bool {
	saved, err := client.GetModuleFile(ctx, targetID, path)
	if err != nil {
		return false
	}
	return acknowledgeIntendedWrite(landed, path, want, saved.Content)
}

// deleteModuleFiles removes what the server holds and the folder does not.
//
// Enumerated from `remote` — everything the server holds — rather than from the
// baseline-recording `landed`, which no longer describes the whole row: a forced
// push past a colleague's added file must still delete it, since that is what
// forcing the folder onto the row means.
func deleteModuleFiles(ctx context.Context, client *api.Client, target *api.Module, local, remote, landed map[string]string, pre filePreconditions, result *modulePushResult) error {
	targetID := target.ID
	deletions := []string{}
	for path := range remote {
		if _, ok := local[path]; !ok {
			deletions = append(deletions, path)
		}
	}
	sort.Strings(deletions)
	for _, path := range deletions {
		if err := client.DeleteModuleFile(ctx, targetID, path, pre.ForDelete(path)); err != nil {
			// Includes the server's refusal to delete __init__.py, whose remedy
			// (empty it rather than remove it) is its message to give — which is
			// why the local guard above refuses a folder missing that file before
			// anything is written.
			if api.StatusOf(err) == api.StatusConflict {
				result.Conflict = true
				noteFileConflict("module", "module", "deleted", path)
			}
			if api.StatusOf(err) == 404 {
				return moduleFileWriteRefusal("delete", path, target, err)
			}
			return fmt.Errorf("delete %s: %w", path, err)
		}
		delete(landed, path)
		result.Deleted = append(result.Deleted, path)
	}
	return nil
}

// moduleFileWriteRefusal explains the one refusal on a file write whose status
// code says nothing at all: a 404.
//
// The server answers 404 — not 403 — when the caller is neither an admin nor the
// draft's own drafter, because enumerating whose draft it is would disclose a
// row they may not see (api/v2/module's requireWritable). A module has ONE
// draft, so this is the ordinary shape of "you are pushing into a colleague's
// draft", and `module status` has already reported the drafter.
//
// It is NOT the only cause — the row may genuinely have been discarded or
// deleted between this push's read and its write — so the message names both,
// leading with the one the drafter id makes likely. Passing the raw 404 through
// would leave the reader looking for a missing file, since that is what a 404 on
// a path-addressed route usually means.
func moduleFileWriteRefusal(verb, path string, target *api.Module, err error) error {
	who := "somebody else"
	if target.DrafterUserID != "" {
		who = "user " + target.DrafterUserID
	}
	return fmt.Errorf("%s %s: the server would not write to draft %s.\n  A module has ONE draft, and this one was started by %s — only they or an admin may write to it, and the refusal is a 404 rather than a 403 so that it discloses nothing about a draft you cannot see. (The other cause is that the draft was discarded or the module deleted while this push was running; `ronja module status` says which.): %w",
		verb, path, target.ID, who, err)
}

func printModulePushReport(r *modulePushResult) {
	out := os.Stdout

	if r.UpToDate {
		fmt.Fprintf(out, "  Up to date — the draft %s already holds this folder.\n", r.DraftID)
		return
	}

	if r.Created {
		fmt.Fprintf(out, "  Created module %s\n", r.ModuleID)
		if r.Proposed {
			// Said at the moment it happens, because it changes what the author
			// has to do next: nothing imports a proposal, and `module publish`
			// submits it rather than publishing it.
			fmt.Fprintf(out, "    proposed — the feature is shared, so an admin must approve this module before\n")
			fmt.Fprintf(out, "               any workflow can import it\n")
		}
	}
	for _, path := range r.Pushed {
		fmt.Fprintf(out, "    pushed   %s\n", path)
	}
	for _, path := range r.Deleted {
		fmt.Fprintf(out, "    deleted  %s\n", path)
	}
	if r.Metadata != "" {
		fmt.Fprintf(out, "    updated  %s\n", r.Metadata)
	}
	if r.Unchanged > 0 {
		fmt.Fprintf(out, "    %d unchanged\n", r.Unchanged)
	}
	if r.MetadataDeferred != "" {
		fmt.Fprintf(out, "    deferred %s\n", r.MetadataDeferred)
	}

	if r.Error != "" {
		fmt.Fprintf(out, "\n  Push stopped part-way: %s\n", r.Error)
		fmt.Fprintf(out, "  The draft %s holds what is listed above; fix the problem and push again.\n", r.DraftID)
		return
	}
	fmt.Fprintf(out, "\n  Draft:    %s\n", r.DraftID)
	if r.Target != "" {
		fmt.Fprintf(out, "  Target:   %s\n", r.Target)
	}
	fmt.Fprintf(out, "\n  Next: ronja module publish\n")
}
