package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The NAME RULE, in the one place every loop reads it from.
//
// A name is an ATTRIBUTE, never an identity: which row a file or a folder is
// comes from the lock, and the name is something a push converges like any
// other field. Every kind follows the same two sentences:
//
//   - A DECLARED name is managed. It is written when the row does not hold it,
//     and it is drift-checked against a baseline first.
//   - An ABSENT name is stamped once, at create, and never written afterwards.
//
// The drift check is three arms, with B the baseline (the name this folder last
// agreed the live row held), L the live name and D the declared one:
//
//	L = D  nothing to write, and the anchor becomes D — live equals desired is
//	       never drift, whatever the anchor said
//	L = B  the folder's own last state, so a different D is a rename to send
//	else   the row was renamed somewhere else; refused as drift, with the
//	       loop's usual override
//
// The baseline is the applied name the lock records (the local baseline for a
// legacy folder). When nothing is recorded it falls back to what the create
// stamped: the file's stem for a table or a metric, the filename for an
// automation. A folder kind's title has no such fallback — it was never derived
// from anything — so with nothing recorded titleLeg asks the head anchor, and
// refuses when the anchor cannot vouch.
//
// ⚠️ The baseline is recorded only in ronja.lock.json. A CI runner that never
// commits the lock back compares its SECOND rename against the stem (or, for a
// folder title, against nothing), reads its own first rename as somebody
// else's, and refuses it; `sync apply` has no --force. Every such refusal names
// the way out (lockRemedy): a push with --force from a checkout, then a commit
// of the lock. See BL-8519.

// nameAction is what the name rule decides about one row.
type nameAction string

const (
	// nameUnmanaged — the file declares no name, so nothing is compared and
	// nothing is written.
	nameUnmanaged nameAction = ""
	// nameAgrees — the row already holds the declared name.
	nameAgrees nameAction = "agrees"
	// nameRename — the row holds the baseline, so the declared name is sent.
	nameRename nameAction = "rename"
	// nameDrift — the row holds neither, so somebody renamed it elsewhere.
	nameDrift nameAction = "drift"
	// nameUnanchored — the row differs from the declared name and there is no
	// baseline at all to tell a rename this folder declared from one made in
	// the web app. Not written. (Every kind decideName serves has a stem to
	// fall back on; the folder kinds' titles answer this case in titleLeg.)
	nameUnanchored nameAction = "unanchored"
)

// nameBaseline is B: what this folder last agreed the live row was called.
type nameBaseline struct {
	// Recorded is the applied name the lock (or a legacy folder's local
	// baseline) holds. Empty is "no answer".
	Recorded string
	// Stem is what the create stamped, consulted only when nothing is
	// recorded: a table's or a metric's file stem, an automation's filename.
	// Empty for a kind with no such fallback.
	//
	// ⚠️ There is deliberately NO "the live name slugs to the stem" leg for a
	// cloned folder. `pipeline clone` writes a table called "Sales orders" to
	// Sales-orders.sql, so the stem is not the name — and accepting any live
	// name whose slug is the stem would read a colleague's punctuation-only
	// rename ("Sales orders" → "Sales: orders") as this folder's own and revert
	// it, silently. A clone RECORDS the live name instead, which is exact; a
	// folder cloned before it did has only the stem, and its first `-- @name`
	// on such a table is refused as drift (declare the current name first, or
	// --force) — the refusing side, never the reverting one.
	Stem string
	// Carried says Stem is not the current file's but the one a `moved` entry
	// carried with the row from its old file (wfdir.LockTable.Stem), so a
	// refusal can say which file it was.
	Carried bool
}

// withCarried is the baseline with a stem a `moved` entry carried, when there is
// one, in place of the current file's.
func (b nameBaseline) withCarried(stem string) nameBaseline {
	if stem != "" {
		b.Stem, b.Carried = stem, true
	}
	return b
}

// matches reports whether a name is this folder's own last state.
func (b nameBaseline) matches(name string) bool {
	if b.Recorded != "" {
		return sameName(name, b.Recorded)
	}
	return b.Stem != "" && sameName(name, b.Stem)
}

// known reports whether there is any baseline at all.
func (b nameBaseline) known() bool { return b.Recorded != "" || b.Stem != "" }

// describe names the baseline for a refusal, so the reader can see what the
// rule compared against.
func (b nameBaseline) describe() string {
	if b.Recorded != "" {
		return fmt.Sprintf("%q, the name this folder last agreed it had", b.Recorded)
	}
	stem := "the file's stem"
	if b.Carried {
		stem = "the stem of the file it had before a \"moved\" entry renamed it"
	}
	return fmt.Sprintf("%q, %s (this folder has no name recorded for it; if the current name is the folder's own, declare that name and push once to adopt it)", b.Stem, stem)
}

// sameName is the rule's equality: what the server stores, which is the name
// trimmed. Case is NOT folded — a declared "Sales orders" against a row called
// "sales orders" is a rename to send, and folding it would leave the row
// disagreeing with the file for ever.
func sameName(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// decideName applies the three arms to one observed name.
func decideName(declared *string, live string, base nameBaseline) nameAction {
	if declared == nil {
		return nameUnmanaged
	}
	switch {
	case sameName(live, *declared):
		return nameAgrees
	case !base.known():
		return nameUnanchored
	case base.matches(live):
		return nameRename
	default:
		return nameDrift
	}
}

// nameDriftReason is the refusal a drifted name earns, in one wording for every
// kind. `what` names the row ("table table-1", "the live table").
func nameDriftReason(what, live, declared string, base nameBaseline) string {
	return fmt.Sprintf("%s is called %q on the server, which is neither the declared %q nor %s — it was renamed somewhere else since",
		what, live, declared, base.describe())
}

// lockRemedy is the way out of a name or title refusal that this repository's
// own deploy can cause. The name a folder last saw is recorded in
// ronja.lock.json, so a deploy from a machine that never commits the lock back —
// a build server — leaves the next deploy comparing against an older name, and
// its own rename reads as one made in Ronja. Nothing on the server tells the two
// apart, so the refusal stands and says how to settle it: one push with --force
// from a checkout, which renames the row as declared and records the name, and
// a commit of the lock. `sync apply` has no --force. A legacy folder (no stack)
// records the name in its local baseline only, so there is nothing to commit.
//
// `command` is the folder loop's push, without the "ronja" ("pipeline push").
func lockRemedy(f *folder, command string) string {
	if f.Stack == "" {
		return fmt.Sprintf("If this folder's own push from another checkout set it, run `ronja %s --force` here to rename it as declared", command)
	}
	return fmt.Sprintf("If this repository's own deploy set it from a machine that did not commit %s afterwards (a build server), run `ronja %s --stack %s --force` from a checkout of this repository — it renames it as declared and records the name — then commit %s (`sync apply` has no --force)",
		wfdir.LockName, command, f.Stack, wfdir.LockName)
}

// nameRemedy is what a name or title refusal offers after "push --force to
// rename it back", as a suffix that starts the refusal's next line(s): in an
// ADOPTED folder, `pull` (f.pullHint) — its baseline is the claim's anchor on
// the server, which every deploy records whether or not the lock is committed,
// so lockRemedy's build-server case cannot arise and the refusal is a real
// rename made elsewhere; in a classic folder, lockRemedy, whose baseline lives
// only in ronja.lock.json (BL-8519).
func nameRemedy(f *folder, command, indent string) string {
	if hint := f.pullHint(indent); hint != "" {
		return hint
	}
	return ".\n" + indent + lockRemedy(f, command)
}

// renameResult is a rename a push sent, for the report: what the row was called
// and what it is called now (or will be, once a staged draft is published).
type renameResult struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// draftRenameWords are the words a table's and a metric's rename step differ
// by: the noun, how the file declares the name, and what to rename instead.
type draftRenameWords struct {
	// noun is what the live row is ("table", "metric").
	noun string
	// change is what the author edits to pick another name ("this file's
	// `-- @name`"), and from is the same carrier in the request's own error.
	change, from string
	// taken is said after "taken in the feature", and holder is what to rename
	// in the web app instead.
	taken, holder string
}

// renameDraft is the declared-name step a table and a metric share: rename the
// DRAFT (never the live row — a commit lands the draft's name on live, so a
// rename written on live while the draft is open would be reverted by the next
// publish) to `want` when it does not hold it already, note it, and record it
// against this draft as the folder's own staged name. A nil result and nil
// error is a draft that already holds the name. The error is the refusal text
// the caller hands its refuse.
func renameDraft(ctx context.Context, client *api.Client, live liveHashes, path, rowID string, draft *api.Table, want string, words draftRenameWords) (*renameResult, error) {
	if sameName(draft.Name, want) {
		return nil, nil
	}
	if err := client.RenameTable(ctx, draft.ID, want); err != nil {
		if why, taken := api.AsNameTaken(err); taken {
			return nil, fmt.Errorf("renaming %s to %q was refused — that name is taken in the feature%s:\n      %s\n    Change %s, or rename %s in the web app, then push again",
				rowID, want, words.taken, why, words.change, words.holder)
		}
		return nil, fmt.Errorf("rename draft %s to %q (from %s): %v", draft.ID, want, words.from, err)
	}
	fmt.Fprintf(os.Stderr, "  Note: %s — renaming %s from %q to %q; it reaches the live %s when this draft is published.\n",
		path, rowID, draft.Name, want, words.noun)
	// Recorded the moment the draft holds it, against THIS draft: the next
	// push's draft leg reads it as the folder's own staged rename.
	live.recordStagedName(path, draft.ID, want)
	return &renameResult{From: draft.Name, To: want}, nil
}

// titledRow is one row a title leg looks at: its id and what it is titled.
//
// RetitledInPlace (the live row only) says the row's title can change without a
// version being filed — a live workflow in a private feature is retitled in
// place (rworkflow.Update) — so no commit precondition catches a rename of it,
// and the title leg must look at it itself.
type titledRow struct {
	ID, Title       string
	RetitledInPlace bool
}

// titleLeg is the name rule for a FOLDER kind's manifest "title" — a data app, a
// workflow, a module — and answers whether the push may write it, or why it
// must not.
//
// The baseline is the title this folder last agreed the row had: the lock's
// applied title (a committed fact, recorded at create, at publish, and whenever
// the live title already equals the declared one), else the local baseline's,
// when that baseline was taken from one of these rows. TWO rows can disagree
// with the manifest — the live one and the draft the push writes to, which is
// what a publish lands; for a row never published they are one — and each is
// held to the same three arms: holding the declared title is never drift,
// holding a baseline title is this folder's own last state, and anything else
// was renamed somewhere else and is refused, overridable with --force.
//
// With NO recorded title at all — a lock written before the applied title
// existed and no local baseline, which is a fresh CI checkout — the one other
// witness is the committed HEAD ANCHOR. headVouches is the caller's answer to
// "does the anchor prove the live title is still this folder's own?", and it
// may only be true for a kind whose title cannot change on the live row without
// moving the head:
//
//   - true: the live title IS the baseline, so a different declared title is a
//     rename to send and the live one is recorded as this folder's own.
//   - false: nothing can tell a rename declared here from one made in the web
//     app, so the push is REFUSED as drift and --force is the override. Never
//     record-then-revert: recording the live title and renaming over it on the
//     next push would revert a colleague's rename one push later instead of
//     at once.
//
// An absent title is unmanaged: stamped at create, never written after.
func titleLeg(f *folder, kind string, live *titledRow, target titledRow, headVouches, force bool) (bool, error) {
	declared := f.Manifest.Title
	if declared == "" {
		return false, nil
	}
	var known []string
	if t := f.Lock.AppliedTitle(f.Stack); t != "" {
		known = append(known, t)
	}
	// ownPending: the draft holds the declared title and the local baseline says
	// THIS folder put it there. A live row still carrying the old title is then
	// just the rename waiting for its publish, not somebody else's.
	ownPending := false
	if b := f.State.For(f.Key); b != nil && b.Title != "" &&
		(b.SourceID == target.ID || (live != nil && b.SourceID == live.ID)) {
		known = append(known, b.Title)
		ownPending = b.SourceID == target.ID && sameName(b.Title, declared) && sameName(target.Title, declared)
	}
	// The live title: the live row's, or — for a row never published, which IS
	// the draft — the draft's own.
	liveTitle := target.Title
	if live != nil {
		liveTitle = live.Title
	}
	// THE HEAD'S WITNESS, consulted only when nothing is recorded: the anchor
	// says nobody has published the live row since this folder last agreed with
	// it, and for this kind only a publish can retitle it — so its title is this
	// folder's own last state.
	vouched := false
	if len(known) == 0 && headVouches {
		known = append(known, liveTitle)
		vouched = true
	}
	isKnown := func(name string) bool {
		for _, k := range known {
			if sameName(name, k) {
				return true
			}
		}
		return false
	}
	// With this folder's own rename pending, the live row is left to the
	// commit's base-version precondition, which catches a live row renamed since
	// the draft was opened — but only where a rename files a version. A row
	// retitled in place (a workflow) moves no version, so its live title is still
	// held to the known titles here, as long as the lock recorded the one it had
	// before the rename: without that record the live row's old title cannot be
	// told from a colleague's, and it is left unchecked rather than refuse the
	// folder's own pending rename (BL-5d0e).
	checkLive := live != nil && live.ID != target.ID &&
		(!ownPending || (live.RetitledInPlace && f.Lock.AppliedTitle(f.Stack) != ""))
	rows := []struct{ what, title string }{{"draft " + target.ID, target.Title}}
	if checkLive {
		rows = append(rows, struct{ what, title string }{"the live " + kind + " " + live.ID, live.Title})
	}
	var moved []string
	for _, row := range rows {
		if sameName(row.title, declared) || isKnown(row.title) {
			continue
		}
		moved = append(moved, fmt.Sprintf("%s is titled %q", row.what, row.title))
	}
	write := !sameName(target.Title, declared)
	resource := target.ID
	if live != nil {
		resource = live.ID
	}
	switch {
	case len(moved) > 0 && len(known) == 0 && !force:
		return false, fmt.Errorf("the title of %s %s does not match %s (%q): %s — and this folder has no recorded title to tell a rename made here from one made in the web app.\n  Set \"title\" in %s to what it is now, remove the key to stop managing it, or push --force to rename it to %q%s",
			kind, resource, wfdir.ManifestName, declared, strings.Join(moved, " and "), wfdir.ManifestName, declared, nameRemedy(f, titlePushCommand(kind), "  "))
	case len(moved) > 0 && !force:
		return false, fmt.Errorf("the title of %s %s was renamed somewhere other than this folder since it last agreed with it (%q): %s.\n  Pushing would rename it back to %q from %s. Set \"title\" there to what it is now, remove the key to stop managing it, or push --force to rename it back%s",
			kind, resource, known[0], strings.Join(moved, " and "), declared, wfdir.ManifestName, nameRemedy(f, titlePushCommand(kind), "  "))
	case len(moved) > 0:
		fmt.Fprintf(os.Stderr, "  Note: --force — renaming to %q: %s.\n", declared, strings.Join(moved, " and "))
		// Acknowledged, so the next push does not re-report it before the
		// rename is published.
		recordAppliedTitle(f, liveTitle)
	case sameName(liveTitle, declared) || isKnown(liveTitle):
		// LIVE EQUALS DESIRED — whatever the anchor said, this is now the title
		// this folder agrees with — or the live row holds this folder's own last
		// state (recorded, or vouched for by the head anchor). Either way it is
		// the title to compare the NEXT push against, and recording it is what
		// lets a push that staged a rename be followed by another before the
		// publish lands.
		if vouched && write {
			fmt.Fprintf(os.Stderr, "  Note: no title was recorded for %s %s, but its version history shows nobody has published it since this folder last agreed with it — so %q is this folder's own, and it is renamed to %q.\n",
				kind, resource, liveTitle, declared)
		}
		recordAppliedTitle(f, liveTitle)
	}
	return write, nil
}

// titlePushCommand is the single-folder push of a folder kind titleLeg serves.
func titlePushCommand(kind string) string {
	switch kind {
	case "data app":
		return "app push"
	case "module":
		return "module push"
	default:
		return "wf push"
	}
}

// recordAppliedTitle records a title as the one this folder agrees the live row
// has, and SAVES it at once: it is a fact taken from a read, true whatever the
// rest of the push does, and a push with nothing else to write would otherwise
// never save the lock. A legacy folder keeps its title in the local baseline
// and records nothing here.
func recordAppliedTitle(f *folder, title string) {
	if f.Stack == "" || f.Lock.AppliedTitle(f.Stack) == title {
		return
	}
	f.Lock.SetAppliedTitle(f.Stack, title)
	if f.Lock.AppliedTitle(f.Stack) != title {
		// The stack has recorded nothing yet, so there is no entry to hold it.
		return
	}
	if err := f.saveFolder(); err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not record the title in %s (%v).\n", wfdir.LockName, err)
	}
}

// reanchorTitleAndHead is the publish-time half: the commit landed the draft's
// title on the live row, and when that is the manifest's, it is the title this
// folder now agrees with — read off the row, never assumed. Then the head
// anchor, as every publish re-takes it.
func reanchorTitleAndHead(ctx context.Context, read headReader, f *folder, liveID, liveTitle string) error {
	if f.Manifest.Title != "" && sameName(liveTitle, f.Manifest.Title) {
		recordAppliedTitle(f, liveTitle)
	}
	return reanchorOnLive(ctx, read, f, liveID)
}
