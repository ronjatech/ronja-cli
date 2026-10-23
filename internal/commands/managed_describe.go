package commands

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The WORDING of an adopted folder's refusals and notes: why the server's
// state could not be read, what each claim state and refused claim item means
// and how to get out of it, and how an address, a row and its holder are named.
// Split from managed.go, which reads and settles the state these describe.

// describeManagedReadError says why GET /managed answered nothing usable.
func describeManagedReadError(err error, f *folder) error {
	if noManagedRoute(err) {
		// The router's plain-text breadcrumb: no such route at all.
		return fmt.Errorf("%s has a folder id, so its state lives on the server — and %s does not serve /api/v2/managed (an instance older than server-side state). Run this folder from a CLI against an instance that does, or `ronja sync unmanage --stack %s` to run it from the lock alone",
			wfdir.ManifestName, f.Resolved.URL, f.Stack)
	}
	if api.WireCodeOf(err) == api.InsufficientScopeCode {
		return fmt.Errorf("%s has a folder id, so its state lives on the server, and this token may not read it: an adopted folder needs the structure:read scope (structure:write to deploy), beside each kind's own. %w",
			wfdir.ManifestName, err)
	}
	return fmt.Errorf("read this folder's state for stack %q (feature %s) from %s: %w", f.Stack, f.Binding.FeatureID, f.Resolved.URL, err)
}

// noManagedRoute reports whether a /managed request met an instance that does
// not serve the routes at all: a 404 whose body is the router's plain-text
// breadcrumb rather than a JSON error. Such an instance holds no server state.
func noManagedRoute(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && !json.Valid([]byte(apiErr.Body))
}

// describeClaimState is the refusal for a claim in a state other than ok —
// the four refusing outcomes, each with its remedy.
func describeClaimState(f *folder, c *api.ManagedClaim) managedRefusal {
	where := describeManagedAddress(f, c.Address)
	unmanage := fmt.Sprintf("`ronja sync unmanage --stack %s`", f.Stack)
	switch c.State {
	case api.ManagedStateNotReadable:
		return managedRefusal{syncReasonNotReadable, fmt.Sprintf(
			"%s is managed by a row you may not read — claimed by %s, typically their unpublished draft. They can publish it, or release the folder's claims themselves with %s; anyone else who may write feature %s can release them with that command and --force (the rows are left as they are, and %s cannot record a row you may not read, so a push of this file afterwards would create a second one)",
			where, orUnknown(c.Claimant), unmanage, c.FeatureID, wfdir.LockName)}
	case api.ManagedStateTrashed:
		return managedRefusal{syncReasonTrashed, fmt.Sprintf(
			"%s is managed by %s, which is in the trash. Restore it in the web app, or release this folder's claims with %s, or declare %q in \"removed\" if the file is gone for good",
			where, orUnknown(c.ResourceID), unmanage, c.Address)}
	case api.ManagedStateLost:
		return managedRefusal{syncReasonLost, fmt.Sprintf(
			"%s is managed by %s, which no longer exists. Release this folder's claims with %s, re-adopt with `ronja sync adopt --stack %s`, and apply with --allow-create to create it again — or declare %q in \"removed\" if the file is gone for good",
			where, orUnknown(c.ResourceID), unmanage, f.Stack, c.Address)}
	case api.ManagedStateMovedOut:
		row, now := describeRowOf(c), describeFeatureNow(c)
		if revivalMissed(c) {
			// The row is BACK in this feature and its claim is still a
			// tombstone: the move back raced the move out, so the revive did
			// not happen. Moving it "back" again is no remedy — it is already
			// here — and nothing else revives a tombstone.
			return managedRefusal{syncReasonMovedOut, fmt.Sprintf(
				"%s holds a tombstone of %s, and that row is back in this stack's feature %s — its claim missed the move back (the two moves raced), so it still only holds the address. Unmanage this address: release this folder's claims with %s (%s keeps the row, since it lives here) and claim it again with `ronja sync adopt --stack %s`",
				where, row, c.FeatureID, unmanage, wfdir.LockName, f.Stack)}
		}
		if c.Departed() {
			back := fmt.Sprintf("Move the row back into feature %s and the claim is live again — or, to", c.FeatureID)
			if !c.Readable && c.ResourceID != "" {
				// The row is gone: there is nothing to move back.
				back = "To"
			}
			return managedRefusal{syncReasonMovedOut, fmt.Sprintf(
				"%s was managed by %s, which was moved to %s; this stack's feature is %s. The claim stays at its address as a tombstone, so nothing is created there in the row's place. %s create the file's row here anew, release this folder's claims with %s, re-adopt with `ronja sync adopt --stack %s` and apply with --allow-create; or declare %q in \"removed\" if the file is gone for good",
				where, row, now, c.FeatureID, back, unmanage, f.Stack, c.Address)}
		}
		return managedRefusal{syncReasonMovedOut, fmt.Sprintf(
			"%s is managed by %s, which now lives in %s, not in this stack's feature %s (its claim raced the move). Move the row back, or release this folder's claims with %s",
			where, row, now, c.FeatureID, unmanage)}
	}
	return managedRefusal{syncReasonClaimRefused, fmt.Sprintf("%s has a claim in state %q, which this CLI does not know — upgrade it (run: ronja update)", where, c.State)}
}

// describeRowOf names a claim's row: its id, or that it is withheld.
func describeRowOf(c *api.ManagedClaim) string {
	if c.ResourceID != "" {
		return c.ResourceID
	}
	return "a row you may not read"
}

// describeFeatureNow names where a moved-out claim's row lives now, as far as
// the server says. It reports the feature only when the caller may read the
// row. A tombstone whose row is GONE reads like a lost claim — unreadable,
// with its id kept (views.go liveView) — and is said as such: its destination
// feature, or the row, was deleted since. Anything else unreadable is a feature
// this credential may not read, or one in the trash, which the answer cannot
// tell apart.
func describeFeatureNow(c *api.ManagedClaim) string {
	switch {
	case c.CurrentFeatureID != "":
		return "feature " + c.CurrentFeatureID
	case c.Departed() && !c.Readable && c.ResourceID != "":
		return "another feature, and it no longer exists there (that feature, or the row, was deleted since)"
	}
	return "another feature (one you may not read, or one that no longer exists)"
}

// revivalMissed reports a TOMBSTONE whose row is back in the claim's own
// feature: the cross-feature move back raced the one out, and the claim was not
// revived. Only a readable row can say where it lives.
func revivalMissed(c *api.ManagedClaim) bool {
	return c.Departed() && c.Readable && c.CurrentFeatureID != "" && c.CurrentFeatureID == c.FeatureID
}

// describeManagedAddress names one address in a message.
func describeManagedAddress(f *folder, address string) string {
	if address == "" {
		return fmt.Sprintf("this %s on stack %q", f.Kind.Label, f.Stack)
	}
	return fmt.Sprintf("%s on stack %q", address, f.Stack)
}

func orUnknown(s string) string { return cmp.Or(s, "(unknown)") }

// joinManagedRefusals folds several refusals into one: the first reason, every
// detail.
func joinManagedRefusals(problems []managedRefusal) *managedRefusal {
	details := make([]string, 0, len(problems))
	for _, p := range problems {
		details = append(details, p.Detail)
	}
	return &managedRefusal{Reason: problems[0].Reason, Detail: strings.Join(details, "\n  ")}
}

// describeClaimRefusal is one refused claim item, in refreshManaged's
// vocabulary.
func describeClaimRefusal(f *folder, a wfdir.ManagedAddress, r api.ManagedClaimResult) managedRefusal {
	where := describeManagedAddress(f, a.Address)
	switch r.Code {
	case api.ManagedCodeResourceClaimed:
		holder := "another address"
		if r.Claim != nil {
			holder = describeHolderAddress(r.Claim)
		}
		return managedRefusal{syncReasonClaimedElsewhere, fmt.Sprintf(
			"%s records %s, and that row is already managed at %s — one row has one address. Point this file at its own row, or release the other claim",
			where, a.ID, holder)}
	case api.ManagedCodeAddressClaimed:
		if revivalMissed(r.Claim) {
			return managedRefusal{syncReasonMovedOut, fmt.Sprintf(
				"%s records %s, and the address holds a tombstone of %s, which is back in this stack's feature — its claim missed the move back. Unmanage this address: release this folder's claims with `ronja sync unmanage --stack %s` and claim it again with `ronja sync adopt --stack %s`",
				where, a.ID, describeRowOf(r.Claim), f.Stack, f.Stack)}
		}
		if r.Claim.Departed() {
			return managedRefusal{syncReasonMovedOut, fmt.Sprintf(
				"%s records %s, and the address holds a tombstone of %s, which was moved to another feature — the address stays taken until it is released. Release this folder's claims with `ronja sync unmanage --stack %s` and re-adopt, or move that row back into feature %s",
				where, a.ID, describeRowOf(r.Claim), f.Stack, f.Binding.FeatureID)}
		}
		return managedRefusal{syncReasonClaimedElsewhere, fmt.Sprintf(
			"%s records %s, and the server's claim at that address names %s — re-run to read the server's state",
			where, a.ID, describeHolder(r.Claim))}
	case api.ManagedCodeMovedOut:
		return managedRefusal{syncReasonMovedOut, fmt.Sprintf(
			"%s records %s, which no longer lives in this stack's feature %s (it was moved to another feature), so it cannot be claimed here. Move it back into feature %s, or drop it from %s and apply with --allow-create to create the file's row here anew",
			where, a.ID, f.Binding.FeatureID, f.Binding.FeatureID, wfdir.LockName)}
	case api.ManagedCodeTrashed:
		return managedRefusal{syncReasonTrashed, fmt.Sprintf(
			"%s records %s, which is in the trash. Restore it in the web app, or declare %q in \"removed\"",
			where, a.ID, a.Address)}
	case "not_found":
		return managedRefusal{syncReasonLost, fmt.Sprintf(
			"%s records %s, which no longer exists or which you may not read, so it cannot be claimed. Drop it from %s and apply with --allow-create to create it again, or have its owner publish it",
			where, a.ID, wfdir.LockName)}
	}
	return managedRefusal{syncReasonClaimRefused, fmt.Sprintf("%s records %s, and the server refused to claim it (%s): %s",
		where, a.ID, orUnknown(r.Code), orUnknown(r.Error))}
}

func describeHolder(c *api.ManagedClaim) string {
	if c == nil {
		return "another row"
	}
	if c.ResourceID != "" {
		return c.ResourceID
	}
	return fmt.Sprintf("a row you may not read (claimed by %s)", orUnknown(c.Claimant))
}

func describeHolderAddress(c *api.ManagedClaim) string {
	addr := c.Address
	if addr == "" {
		addr = "(the folder)"
	}
	return fmt.Sprintf("%s of folder %s (stack label %q, %s)", addr, c.FolderID, c.StackLabel, orUnknown(c.SourceLabel))
}

// managedRewriteNotes says what the refresh rewrote, one note per address —
// the lock changing under a person is something they should see. One wording,
// for the stderr of a single-folder command and for a tree command's folder
// line alike.
func (f *folder) managedRewriteNotes() []string {
	if f.managed == nil {
		return nil
	}
	notes := make([]string, 0, len(f.managed.rewrites))
	for _, r := range f.managed.rewrites {
		notes = append(notes, fmt.Sprintf("%s — %s", describeManagedAddress(f, r.Address), r.describe()))
	}
	return notes
}

// describe is one rewrite, for a note: a different row, or the same row with
// a drift baseline the server recorded and the lock did not.
func (r managedRewrite) describe() string {
	if r.HeldAt != "" {
		return fmt.Sprintf("%s recorded %s here, and the server's state holds that row at %s (a \"moved\" another run applied); this entry was dropped", wfdir.LockName, r.From, r.HeldAt)
	}
	if r.From == r.To && r.To != "" {
		return fmt.Sprintf("%s's drift baseline for %s differed from the one the server recorded, and was rewritten from the server", wfdir.LockName, r.To)
	}
	return fmt.Sprintf("%s recorded %s, the server's state says %s; rewritten from the server", wfdir.LockName, orNothing(r.From), orNothing(r.To))
}
