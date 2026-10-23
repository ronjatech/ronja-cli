package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// ANCHORS: what an adopted folder records on the server after every lock save
// — the folder's own anchor first (its docs-sidecar and health-checks
// baselines), then each claim's — and how a write merges with what a newer CLI
// recorded, survives a concurrent apply, and reports a failure. Split from
// managed.go, which reads and settles the state these anchors belong to.

// mergeAnchor is the anchor to write for one claim: the lock's fields for this
// kind, over whatever the claim already holds — so a key a NEWER CLI recorded
// survives this one's write. A key this build knows and the lock leaves empty
// is cleared. A RETIRED key (wfdir.RetiredAnchorKeys — the docs records the
// folder anchor now holds) is cleared only when dropRetired says its new home
// holds it; until then it is carried, so a folder anchor that failed to record
// never costs the only server-side copy.
func mergeAnchor(prev map[string]any, kind string, fromLock map[string]any, dropRetired bool) map[string]any {
	out := mergeAnchorKeys(prev, wfdir.AnchorKeys(kind), fromLock)
	if dropRetired {
		for _, k := range wfdir.RetiredAnchorKeys(kind) {
			delete(out, k)
		}
	}
	return out
}

// mergeAnchorKeys writes fromLock over prev for the keys this build owns,
// keeping every other key prev carries.
func mergeAnchorKeys(prev map[string]any, owned []string, fromLock map[string]any) map[string]any {
	out := map[string]any{}
	maps.Copy(out, prev)
	for _, k := range owned {
		delete(out, k)
	}
	maps.Copy(out, fromLock)
	return out
}

// canonicalJSON is a map's encoding with sorted keys — the comparison every
// anchor diff uses, so a nil map and an empty one agree.
func canonicalJSON(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	// Re-decode and re-encode, so values that came off the wire and values built
	// in code spell numbers and nested objects the same way.
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// recordManagedAnchors writes every anchor the lock holds and the server does
// not: after each lock save, best-effort. It never
// fails the save that triggered it — the local half of that save landed, and a
// "does not name it" refusal about a lock that does name it would be a lie —
// so the first failure is kept on the state and reported at the end of the
// command (managedFinish, and sync apply's run).
func (f *folder) recordManagedAnchors() {
	st := f.managed
	if st == nil || f.ctx == nil || st.readOnly {
		return
	}
	if err := f.writeManagedAnchors(f.ctx); err != nil && st.anchorErr == nil {
		st.anchorErr = err
	}
}

// writeManagedAnchors writes the FOLDER's anchor first and then each claim's.
// The two are independent on purpose: the folder anchor depends on no claim,
// so a claim that is refused, removed, unclaimable or skipped cannot cost the
// docs-sidecar baselines, and a folder anchor that fails cannot stop a claim's
// anchor being recorded. The first failure of either is returned.
func (f *folder) writeManagedAnchors(ctx context.Context) error {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	folderErr := f.writeFolderAnchor(ctx, client)
	if err := f.writeClaimAnchors(ctx, client, folderErr == nil); err != nil {
		return err
	}
	return folderErr
}

func (f *folder) writeClaimAnchors(ctx context.Context, client *api.Client, dropRetired bool) error {
	st := f.managed
	addrs := f.Lock.ManagedAddresses(f.Stack, f.Kind)
	relisted := false
	for _, a := range addrs {
		if st.unclaimable[a.Address] {
			continue
		}
		c := st.claims[a.Address]
		if c == nil && !relisted {
			// A create that claimed in its own transaction does not answer with
			// the claim, so a row this command made is learned here.
			relisted = true
			claims, err := client.ListManaged(ctx, st.featureID, st.folderID)
			if err != nil {
				return fmt.Errorf("read this folder's claims to record their anchors: %w", err)
			}
			for i := range claims {
				if claims[i].State == api.ManagedStateOK {
					if _, known := st.claims[claims[i].Address]; !known {
						cl := claims[i]
						st.claims[cl.Address] = &cl
					}
				}
			}
			c = st.claims[a.Address]
		}
		if c == nil && !f.addressOnDisk(a.Address) {
			// A lock entry for a file that is gone: not backfilled (see
			// managedBackfill), so there is no claim to anchor.
			continue
		}
		if c == nil {
			// Still none: a row the lock records that no claim names — claimed
			// now, as the lazy backfill would.
			if err := f.claimManagedRows(ctx, client, []wfdir.ManagedAddress{a}); err != nil {
				return err
			}
			if c = st.claims[a.Address]; c == nil {
				continue
			}
		}
		if c.ResourceID != a.ID {
			return fmt.Errorf("%s records %s at %s, and the server's claim there names %s — re-run to read the server's state", wfdir.LockName, a.ID, describeManagedAddress(f, a.Address), orUnknown(c.ResourceID))
		}
		want := mergeAnchor(c.Applied, a.Kind, f.Lock.Anchor(f.Stack, f.Kind, a.Address), dropRetired)
		if canonicalJSON(want) == canonicalJSON(c.Applied) {
			continue
		}
		updated, err := f.putManagedAnchor(ctx, client, a.Address, c, want)
		if err != nil {
			return err
		}
		st.claims[a.Address] = updated
	}
	return nil
}

// writeFolderAnchor records this folder's anchor — the lock's folder-level
// state (wfdir.Lock.FolderAnchor: the docs-sidecar and health-checks baselines) over whatever the
// anchor already holds, so a key a newer CLI recorded survives — when it
// differs from the anchor as last read or written. One PUT
// /managed/folder/applied, a CAS on the anchor's own revision (0 creates it).
//
// A 409 stale_revision re-reads ONCE: an anchor that already holds this state
// is done; one that still holds the state this write was based on (only its
// revision moved) is retried at the new revision; anything else is another
// apply's different record, surfaced as anchor_not_recorded rather than written
// over. A 413 is the server refusing a state over its cap — nothing was
// written — and is named, never dropped.
func (f *folder) writeFolderAnchor(ctx context.Context, client *api.Client) error {
	st := f.managed
	if st.folder == nil {
		st.folder = &api.ManagedFolderAnchor{FeatureID: st.featureID, FolderID: st.folderID, Applied: map[string]any{}}
	}
	if st.folderTooLarge != nil {
		// Refused once for size this command: every later save would only send
		// a larger body to the same answer.
		return st.folderTooLarge
	}
	base := st.folder
	want := mergeAnchorKeys(base.Applied, wfdir.FolderAnchorKeys(), f.Lock.FolderAnchor(f.Stack))
	if canonicalJSON(want) == canonicalJSON(base.Applied) {
		return nil
	}
	put := func(revision int64) (*api.ManagedFolderAnchor, error) {
		return client.RecordManagedFolderApplied(ctx, api.ManagedFolderAppliedInput{
			FeatureID: st.featureID, FolderID: st.folderID, Applied: want, Revision: revision,
		})
	}
	updated, err := put(base.Revision)
	if err != nil && api.WireCodeOf(err) == api.ManagedCodeStaleRevision {
		state, rerr := client.ReadManaged(ctx, st.featureID, st.folderID)
		if rerr != nil {
			return &managedRefusal{syncReasonAnchorNotRecorded, fmt.Sprintf(
				"this folder's anchor on stack %q was written by another apply while this one ran, and reading it again failed (%v) — the changes landed, and the folder's docs-sidecar and health-checks baselines were not recorded. Re-run to read the server's state",
				f.Stack, rerr)}
		}
		current := state.Folder
		switch canonicalJSON(current.Applied) {
		case canonicalJSON(want):
			st.folder = current
			return nil
		case canonicalJSON(base.Applied):
			updated, err = put(current.Revision)
		default:
			return &managedRefusal{syncReasonAnchorNotRecorded, fmt.Sprintf(
				"another apply recorded a different anchor for this folder on stack %q while this one ran (its docs-sidecar and health-checks baselines), so this one's was not written over it. The changes landed; re-run — the next apply reads the server's anchor, and documentation that already says what the folder declares is not drift",
				f.Stack)}
		}
	}
	switch {
	case err == nil:
		st.folder = updated
		return nil
	case api.WireCodeOf(err) == api.ManagedCodeAppliedTooLarge:
		st.folderTooLarge = &managedRefusal{syncReasonFolderAnchorTooLarge, fmt.Sprintf(
			"this folder's anchor on stack %q — the drift baselines of its %d docs %s and %d health-checks %s — is over the server's cap (%s), so NONE of it was recorded. The writes landed; a runner without %s has no baseline for these sidecars and checks, and would write their files over documentation or checks edited in the app. Split them across more than one folder",
			f.Stack, len(f.Lock.Stacks[f.Stack].TableDocs), plural(len(f.Lock.Stacks[f.Stack].TableDocs), "sidecar"),
			len(f.Lock.Stacks[f.Stack].Checks), plural(len(f.Lock.Stacks[f.Stack].Checks), "file"), api.ErrorMessage(err), wfdir.LockName)}
		return st.folderTooLarge
	case api.WireCodeOf(err) == api.ManagedCodeStaleRevision:
		return &managedRefusal{syncReasonAnchorNotRecorded, fmt.Sprintf(
			"this folder's anchor on stack %q moved again while this apply retried its write, so its docs-sidecar and health-checks baselines were not recorded. The changes landed; re-run to read the server's state",
			f.Stack)}
	}
	return fmt.Errorf("record this folder's anchor (its docs-sidecar and health-checks baselines) on stack %q: %w", f.Stack, err)
}

// putManagedAnchor is one PUT /managed/:id/applied, a CAS on the claim's
// revision. A 409 stale_revision re-reads ONCE: when the claim still names the
// same row and either already holds this anchor or still holds the one this
// write was based on (only its revision moved — a relabel), the write still
// applies and is retried; otherwise another apply recorded a different anchor
// meanwhile, and that is surfaced rather than overwritten.
//
// A 404 is the server's answer both for a claim released meanwhile and for a
// row this credential may no longer write (unreadable, or purged) — it does
// not say which, so the claim is re-read and its actual state named.
//
// The write carries this folder's SOURCE LABEL when the claim's is empty or
// names the same repository at another path (a folder moved inside it) — the
// quiet relabel managed_source.go promises. A different repository is a
// foreign claim, which only --adopt-foreign relabels.
func (f *folder) putManagedAnchor(ctx context.Context, client *api.Client, address string, c *api.ManagedClaim, want map[string]any) (*api.ManagedClaim, error) {
	gone := func() error {
		current := f.rereadManagedClaim(ctx, address)
		switch {
		case current == nil:
			return &managedRefusal{syncReasonLost, fmt.Sprintf(
				"the server holds no claim at %s any more — it was released while this ran. The change landed, and this folder no longer manages the row; run `ronja sync adopt --stack %s` to claim it again",
				describeManagedAddress(f, address), f.Stack)}
		case current.State != api.ManagedStateOK:
			r := describeClaimState(f, current)
			r.Detail = "the change landed, and its anchor was not recorded: " + r.Detail
			return &r
		}
		return &managedRefusal{syncReasonAnchorNotRecorded, fmt.Sprintf(
			"the server answered 404 to the anchor of %s, and its claim still reads ok — the change landed, and its anchor was not recorded. Re-run to read the server's state",
			describeManagedAddress(f, address))}
	}
	updated, err := client.RecordManagedApplied(ctx, c.ID, f.appliedInput(c, want))
	switch {
	case err == nil:
		return updated, nil
	case api.StatusOf(err) == http.StatusNotFound:
		return nil, gone()
	case api.WireCodeOf(err) == api.ManagedCodeMovedOut:
		return nil, anchorMovedOut(f, address)
	case api.WireCodeOf(err) != api.ManagedCodeStaleRevision:
		return nil, fmt.Errorf("record the anchor of %s: %w", describeManagedAddress(f, address), err)
	}
	current := f.rereadManagedClaim(ctx, address)
	switch {
	case current == nil:
		return nil, gone()
	case current.Departed():
		return nil, anchorMovedOut(f, address)
	case current.ResourceID != c.ResourceID:
		return nil, &managedRefusal{syncReasonAnchorNotRecorded, fmt.Sprintf(
			"%s now names %s on the server, not %s — another apply re-pointed it while this one ran. Re-run to read its state",
			describeManagedAddress(f, address), orUnknown(current.ResourceID), c.ResourceID)}
	case canonicalJSON(current.Applied) == canonicalJSON(want):
		return current, nil
	case canonicalJSON(current.Applied) == canonicalJSON(c.Applied):
		updated, err = client.RecordManagedApplied(ctx, current.ID, f.appliedInput(current, want))
		if err != nil {
			if api.StatusOf(err) == http.StatusNotFound {
				return nil, gone()
			}
			if api.WireCodeOf(err) == api.ManagedCodeMovedOut {
				return nil, anchorMovedOut(f, address)
			}
			return nil, fmt.Errorf("record the anchor of %s: %w", describeManagedAddress(f, address), err)
		}
		return updated, nil
	}
	return nil, &managedRefusal{syncReasonAnchorNotRecorded, fmt.Sprintf(
		"the change to %s landed, and another apply recorded a different anchor for it while this one ran, so this one's was not written over it. Re-run: the next apply reads the server's anchor, and a row that already holds this folder's content is not drift",
		describeManagedAddress(f, address))}
}

// appliedInput is one anchor write against a claim as read: its revision, the
// anchor, and this folder's source label when the claim's needs it (see
// putManagedAnchor).
func (f *folder) appliedInput(c *api.ManagedClaim, want map[string]any) api.ManagedAppliedInput {
	in := api.ManagedAppliedInput{ExpectedRevision: c.Revision, Applied: want}
	if src := f.managed.source; relabelTo(c.SourceLabel, src) {
		in.SourceLabel = &src
	}
	return in
}

// anchorMovedOut is a PUT applied answered 409 moved_out: the claim became a
// tombstone, or its row left the claim's feature, while this ran.
func anchorMovedOut(f *folder, address string) error {
	return &managedRefusal{syncReasonMovedOut, fmt.Sprintf(
		"the change to %s landed, and its row was moved to another feature while this ran, so no anchor was recorded — the claim only holds its address now. Re-run: the next apply reads the claim's state and names the remedy",
		describeManagedAddress(f, address))}
}
