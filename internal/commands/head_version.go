package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The head-version anchor: the committed answer to "has anybody published since
// this folder last agreed with the live row", for the two kinds whose file
// fingerprints describe a per-user draft and so can never be committed.
//
// It exists for one case, and the case is the whole point of the lock file: a
// FRESH CHECKOUT — CI, or a colleague's `git clone` — has no `.ronja/state.json`
// and therefore no baseline at all. Before this, `wf push` and `app push`
// refused such a checkout outright and named `--force` as the way through, which
// is the flag that ALSO disarms the per-file compare-and-swap. The documented CI
// path was the destructive one. A folder that can say which version it forked
// from does not need it: recorded == current means nothing was published in
// between, and the push proceeds under preconditions rather than past them.
//
// A pipeline needs none of this — its live SQL fingerprint (wfdir.LockTable)
// already answers the same question about the same kind of row — which is why
// this file is workflow/data-app only.

// headAgreement is what the committed head pointer says about the live row,
// read ONCE per push and handed to the (otherwise local, otherwise pure) drift
// guard as a value.
//
// A value rather than a callback because the guard is a local comparison that
// several tests drive directly: pushing the round trip out to the caller keeps
// exactly one network read per push and keeps the guard testable without a
// server.
type headAgreement struct {
	// Recorded is what the lock says this folder forked from, empty when there
	// is nothing recorded: a legacy instances[] folder (nowhere to keep it), a
	// stack written by a CLI older than the field, or a clone that declined to
	// claim one. Empty NEVER means agreement — it means the guard has no answer,
	// which is the refusal it always was.
	Recorded string
	// Current is the head the server reports now, empty when it was not read
	// (nothing recorded, so nothing to compare) or when the read failed.
	Current string
	// AnchoredDraft reports that the row this push writes to is one the pointer
	// can actually speak for. Two shapes qualify, and they are the same claim
	// about different rows:
	//
	//   - No row of ours was open, so THIS push checks a draft out of the live
	//     row: its files are that version's, which is what the pointer names.
	//   - The binding names a PARENTLESS draft — a resource this folder created
	//     and has never published. There is no live version behind it and no
	//     edit shadow over it: the row IS the resource, and an unversioned row's
	//     head is its own id, which is exactly what the anchor holds. The moment
	//     somebody publishes it, the head moves and Moved() fires.
	//
	// It is load-bearing, not belt-and-braces, and what it still excludes is the
	// case it exists for: an edit shadow over a LIVE row that was already open
	// when this push started. The pointer is a fact about the live row and says
	// nothing whatever about a half-finished draft forked from an older version,
	// which a vouched push would overwrite without ever comparing anything. That
	// draft falls through to the ordinary refusal.
	AnchoredDraft bool
}

// Vouches reports that this folder may be held to the head pointer instead of a
// local baseline: it recorded one, the live row still sits on it, and the row
// being written is one the pointer describes (see AnchoredDraft).
func (h headAgreement) Vouches() bool {
	return h.Recorded != "" && h.Recorded == h.Current && h.AnchoredDraft
}

// Moved reports a live row that was published to since this folder agreed with
// it — real drift, and the one thing the anchor exists to catch.
func (h headAgreement) Moved() bool {
	return h.Recorded != "" && h.Current != "" && h.Recorded != h.Current
}

// headReader reads the current head for one resource id. The two kinds differ
// only in which route they call, so every caller below takes one of these rather
// than a client and a kind.
type headReader func(ctx context.Context, resourceID string) (string, error)

func workflowHeadReader(client *api.Client) headReader { return client.HeadVersionID }
func dataAppHeadReader(client *api.Client) headReader  { return client.DataAppHeadVersionID }

// readHeadAgreement assembles the anchor for a push.
//
// It costs ONE extra round trip, and only on a STACK folder: a legacy
// instances[] folder has nowhere to keep an anchor, so it makes exactly the
// requests it always did. The trip is paid even when nothing is recorded yet,
// which is what lets a stack folder cloned before this field existed ACQUIRE one
// on its next ordinary push instead of only on a re-clone — see anchorAfterPush
// for the condition that makes that honest.
//
// A failed read is NOT fatal and NOT an agreement: Current stays empty, Vouches
// is false, and the push falls back to the baseline guard it had before. The
// alternative — failing the push — would make a transient 5xx on a listing route
// the reason a deploy stopped, for a check whose absence is simply the older,
// stricter behaviour.
func readHeadAgreement(ctx context.Context, read headReader, f *folder, resourceID string, anchoredDraft bool) headAgreement {
	h := headAgreement{Recorded: f.headVersion(), AnchoredDraft: anchoredDraft}
	if f.Stack == "" || resourceID == "" {
		return h
	}
	current, err := read(ctx, resourceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not read the version history of %s (%v) — falling back to the local sync baseline.\n",
			resourceID, err)
		return h
	}
	h.Current = current
	return h
}

// anchorAfterPush records the live version this folder now sits on, and
// persists it — the lock is a committed file, so a pointer left in memory is a
// pointer the next checkout does not have.
//
// The condition is the honest part, and each leg is a different claim:
//
//   - AnchoredDraft. The row this push wrote to descends from Current — either
//     because this push forked it off the live row moments ago, or because it is
//     a parentless draft whose own id IS the head. This is the leg that gives an
//     existing folder its first anchor.
//   - Recorded == Current. Nothing moved; re-stating what is already there.
//
// Everything else records NOTHING, and the case that rules out is the one that
// matters: a draft that was ALREADY open when this push started may have been
// forked from an older version, and stamping the current head onto a folder
// whose content never saw it is how a later CI push would silently overwrite the
// publish in between.
//
// ⚠️ --force past a MOVED head CLEARS the anchor rather than advancing it, and
// that leg OVERRIDES both of the above — it is checked first precisely because
// the case that bites is a forced push whose target draft this push did fork,
// which satisfies AnchoredDraft and would otherwise advance.
//
// Forcing past a moved head means: somebody published Current, this folder's
// committed content descends from Recorded, and the push was told to overwrite
// the difference rather than to absorb it — so Current's content is NOT in this
// repository, whichever row the bytes landed in. Advancing would make the next
// fresh checkout VOUCH on a version the folder never saw and overwrite it with
// preconditions that pass, converting a refusal into exactly the silent
// overwrite the anchor exists to prevent, and disarming it for everyone who
// migrates off the old --force-based CI path. Empty disarms the "re-report the
// same difference forever" nuisance just as well, and degrades to the pre-anchor
// refusal instead of to a false vouch.
//
// The pipeline analogy does NOT carry here, and it is worth saying why: `ronja
// pipeline push --force` re-records the fingerprint of the live row it just
// WROTE. A forced workflow or data-app push writes a draft and leaves live
// alone, so there is no row it could honestly re-record a pointer for.
func anchorAfterPush(f *folder, h headAgreement, forced bool) error {
	// A legacy folder short-circuits BEFORE the save, not merely at
	// setHeadVersion. Rewriting a v1 manifest is byte-identical and therefore
	// harmless — but "harmless" is a property of SaveManifest that this function
	// should not be relying on, and the one thing a legacy push promises is that
	// it touches nothing.
	if f.Stack == "" || h.Current == "" {
		return nil
	}
	next := h.Current
	switch {
	case forced && h.Recorded != h.Current:
		next = ""
	case h.AnchoredDraft || h.Recorded == h.Current:
		// next stays h.Current.
	default:
		return nil
	}
	if f.headVersion() == next {
		return nil
	}
	f.setHeadVersion(next)
	if err := f.saveFolder(); err != nil {
		return fmt.Errorf("record the version %s now sits on in %s: %w", f.Stack, f.bindingFiles(), err)
	}
	if next == "" {
		fmt.Fprintf(os.Stderr, "  Note: --force pushed past a version this folder had not seen, so the %s anchor for %s was cleared rather than moved onto it — a later checkout will ask for a baseline instead of vouching for %s.\n",
			wfdir.LockName, f.Stack, h.Current)
	}
	return nil
}

// cloneAnchor resolves the version a freshly cloned WORKFLOW folder descends
// from. A data app has its own — see dataAppCloneAnchor, and the ⚠️ there for
// why one function cannot serve both.
//
// identityID is the STABLE row (the live workflow), sourceID the row the files
// were actually read from, and sourceBase that row's own baseVersionID when it
// is a draft.
//
//   - Cloned from LIVE: the current head. The files are that version's.
//   - Cloned from a DRAFT: the draft's base. Deliberately not the current head —
//     see the ⚠️ at the call site. This is the whole reason the CLI decodes
//     baseVersionID at all.
//   - Cloned from a draft with no base: an unversioned row, whose head is its
//     own id.
//
// A read failure returns the error rather than an empty anchor. Clone is the one
// command that can afford to fail — nothing has been created and the remedy is
// to run it again — and a folder that silently came down without an anchor would
// only reveal it as a --force demand in CI, weeks later and to somebody else.
func cloneAnchor(ctx context.Context, read headReader, identityID, sourceID, sourceBase string) (string, error) {
	if sourceID != identityID {
		if sourceBase != "" {
			return sourceBase, nil
		}
		return identityID, nil
	}
	return read(ctx, identityID)
}

// dataAppCloneAnchor is cloneAnchor for a data app, and it is a separate
// function because the two servers mean different things by base_version_id.
//
// ⚠️ rworkflow stamps the HEAD VERSION ROW's id (rworkflow.resolveHeadVersionID,
// falling back to the parent's own id when the workflow has no versions), so a
// workflow draft's base is directly comparable with what HeadVersionID answers.
// rdataapp's ordinary checkout stamps optional.Value(parent.ID) — the LIVE APP'S
// OWN ID — for every draft, versioned or not (rdataapp.buildDraftFromParent).
// Feeding that to the shared function on a versioned app records "app-1" as the
// anchor while DataAppHeadVersionID answers "dav-N" forever: a folder that can
// never push again from a fresh checkout, refused with a message naming an app
// id where a version id belongs.
//
// The one rdataapp path that DOES stamp a real version is RestoreFromVersion,
// and it is told apart by the value itself: a base equal to the identity id is
// the parent-id sentinel, anything else is a genuine version snapshot.
//
// So, in order:
//
//   - Cloned from LIVE: the current head, exactly as for a workflow.
//   - Cloned from a RESTORE draft: that version, which is what the column means
//     there and is comparable with the head.
//   - Cloned from an ordinary draft of an app with NO versions: the app's own
//     id, which is both what the column says and what the head reader answers.
//   - Cloned from an ordinary draft of a VERSIONED app: no anchor at all. Which
//     version those bytes forked from is not recorded anywhere the CLI can read,
//     and the two wrong answers are both worse than none — the current head
//     vouches for a publish this folder never saw (see anchorAfterPush's ⚠️),
//     and the app id can never match. Empty degrades to the pre-anchor refusal,
//     which is a nuisance rather than a silent overwrite.
func dataAppCloneAnchor(ctx context.Context, read headReader, identityID, sourceID, sourceBase string) (string, error) {
	if sourceID == identityID {
		return read(ctx, identityID)
	}
	if sourceBase != "" && sourceBase != identityID {
		return sourceBase, nil
	}
	head, err := read(ctx, identityID)
	if err != nil {
		return "", err
	}
	if head == identityID {
		return head, nil
	}
	return "", nil
}

// anchorPolicy is what a baseline refresh does with the COMMITTED anchor, named
// rather than passed as a bare bool because the two call sites read identically
// and only one of them may move the pointer. See reanchorOnLive's ⚠️.
type anchorPolicy bool

const (
	// anchorOnLive: this command produced the live row's current version, so the
	// folder genuinely agrees with it.
	anchorOnLive anchorPolicy = true
	// keepAnchor: the local baseline was refreshed, but nothing here established
	// that the folder's FILES descend from live's current version.
	keepAnchor anchorPolicy = false
)

// reanchorOnLive re-records the anchor at the moments a folder is put back into
// agreement with the LIVE row: a publish that just committed a new version, and
// a publish that overwrote one. It rides the baseline refresh those already do,
// because it is the same event.
//
// ⚠️ A DISCARD is deliberately NOT one of those moments, and it used to be. A
// discard throws the DRAFT away and leaves the folder's files exactly as they
// are — so if a colleague published while that draft was open, re-anchoring on
// live stamps a version whose content the committed files do not contain. Commit
// that lock and the next fresh checkout vouches on it and overwrites their
// publish with preconditions that pass, which is anchorAfterPush's forced-leg
// bug arriving through a quieter door. Discard therefore passes keepAnchor: the
// pointer stays where it was, and the next baseline-less push refuses naming
// both versions, which is the truth.
//
// A failure is returned to be degraded into a note, never to fail the command —
// the publish has HAPPENED by the time this runs. Leaving the old anchor in
// place is the safe residue: an anchor that is behind refuses a later
// baseline-less push, which is a nuisance, where one that is ahead would vouch
// for a version this folder never saw.
func reanchorOnLive(ctx context.Context, read headReader, f *folder, liveID string) error {
	if f.Stack == "" {
		return nil
	}
	head, err := read(ctx, liveID)
	if err != nil {
		return fmt.Errorf("re-read the current version of %s: %w", liveID, err)
	}
	if f.headVersion() == head {
		return nil
	}
	f.setHeadVersion(head)
	if err := f.saveFolder(); err != nil {
		return fmt.Errorf("write %s: %w", wfdir.LockPath(f.Root), err)
	}
	return nil
}

// headVersion is this folder's recorded anchor for the stack it is acting on.
//
// Empty for a LEGACY instances[] folder, always: there is no committed file to
// keep it in, and inventing one in `.ronja/` would put the environment's answer
// back in the per-user place this whole slice moved it out of. That is the same
// "v1 stays v1" line liveHashes draws, decided the same way.
func (f *folder) headVersion() string {
	if f.Stack == "" {
		return ""
	}
	return f.Lock.HeadVersion(f.Stack)
}

// setHeadVersion records the live version this folder now agrees with, and is a
// NO-OP on a legacy folder for the reason above. Callers do not branch on the
// folder's shape; they say when agreement is true and this decides whether there
// is anywhere to write it.
//
// It writes to the in-memory lock only. Persisting is the caller's own
// saveFolder, so the pointer lands in the same write as the binding it describes
// rather than in one of its own.
func (f *folder) setHeadVersion(versionID string) {
	if f.Stack == "" || f.Lock == nil {
		return
	}
	f.Lock.SetHeadVersion(f.Stack, versionID)
}

// refuseHeadMoved is the message a moved head produces. One function because
// both kinds say the same thing and a fresh checkout is the reader in both
// cases: it has no local detail to offer, so the refusal names the two version
// ids and the two ways out, and does NOT suggest --force — reaching for it here
// would overwrite the very publish this refusal just found.
func refuseHeadMoved(kind, resourceID string, h headAgreement, cloneCmd string) error {
	return fmt.Errorf("%s %s has been published to since this folder last agreed with it: it was at version %s and is now at %s.\n  This checkout has no local sync baseline, so nothing here can tell your change from theirs.\n  Look at what they published, then %s to re-apply your work on top of it",
		kind, resourceID, h.Recorded, h.Current, cloneCmd)
}

// noteHeadAnchored explains, once, that a baseline-less push proceeded on the
// committed anchor rather than on a baseline it does not have.
//
// Printed because the alternative is indistinguishable from the refusal this
// replaces: the reader of a CI log needs to see WHICH guard let the push
// through, or the first thing they do when something goes wrong is assume there
// was none.
func noteHeadAnchored(resourceID, versionID string) {
	fmt.Fprintf(os.Stderr, "  Note: no local sync baseline here, but %s is still at version %s — the version %s recorded. Pushing against the live files.\n",
		resourceID, versionID, wfdir.LockName)
}
