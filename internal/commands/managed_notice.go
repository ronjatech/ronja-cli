package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// THE CLASSIC-LOOP NOTICE: anyone may still clone, push and
// publish a row a repository deploys — the classic pattern keeps working on a
// managed row — and they are TOLD, never blocked. The fact comes from the row's
// own single-row GET, which carries `managedBy` for a row with a live claim
// (backend/resource/rmanaged/managed_by.go), so the
// notice costs no request of its own: every command below already reads that
// row.
//
// The one folder that is NOT told is the one that manages the row: the same
// folder id deploying to the same feature. That is the serious pattern doing
// its job, and a notice there would be noise on every deploy.
//
// A notice is printed to stderr — one line per managing folder, however many of
// its rows the command touched — and carried in --json as `managedInGit`, one
// entry per row. An instance older than the field sends none and the command
// says nothing: absence is never an error.

// managedNotice is one row the command touched that a repository manages.
type managedNotice struct {
	// Kind is the claim's kind as the folder kinds name it: workflow, dataapp,
	// module, table, metric or automation.
	Kind       string `json:"kind"`
	ResourceID string `json:"resourceID"`
	api.ManagedBy
	// Notice is the sentence printed to stderr, so a machine reader can show the
	// same words.
	Notice string `json:"notice"`
}

// managedNoticeVerb says what the notice is about: the change a push or publish
// makes, or the copy a clone just made.
type managedNoticeVerb int

const (
	noticeForChange managedNoticeVerb = iota
	noticeForClone
)

// noticeManagedBy answers the notice for one row, nil when there is nothing to
// say: the GET carried no `managedBy` (an unmanaged row, or an older server),
// or `f` is the folder that manages the row.
//
// `f` is nil for a clone, which writes a NEW folder: a clone never mints a
// folder id, so the folder it writes is never the one that manages
// anything.
func noticeManagedBy(f *folder, kind wfdir.Kind, claimKind, resourceID string, mb *api.ManagedBy, verb managedNoticeVerb) *managedNotice {
	if mb == nil || f.managesClaim(mb) {
		return nil
	}
	return &managedNotice{
		Kind:       claimKind,
		ResourceID: resourceID,
		ManagedBy:  *mb,
		Notice:     describeManagedBy(kind, mb, verb),
	}
}

// managesClaim reports whether this folder is the one a `managedBy` names: the
// same folder id, deploying to the same feature. A stack's NAME is not part of
// an address — two stacks of one folder targeting one feature deploy the same
// thing — so it does not decide. A stack marked "unmanaged" is not the owner
// whatever its id: it deploys the classic way, so a deploy from it is a classic
// edit of the row like any other, and is told so.
func (f *folder) managesClaim(mb *api.ManagedBy) bool {
	if f == nil || f.Manifest == nil || f.Manifest.ID == "" {
		return false
	}
	if f.Stack != "" && f.Manifest.Stacks[f.Stack].Unmanaged {
		return false
	}
	return f.Manifest.ID == mb.FolderID && f.Binding.FeatureID == mb.FeatureID
}

// describeManagedBy is the notice's one line.
func describeManagedBy(kind wfdir.Kind, mb *api.ManagedBy, verb managedNoticeVerb) string {
	var where []string
	repository, folderPath := api.SplitSourceLabel(mb.SourceLabel)
	switch {
	case repository == "":
		// A deploy made from outside git records no label; the folder id is
		// the one name the claim still has.
		where = append(where, "folder id "+mb.FolderID)
	case folderPath == "":
		where = append(where, repository)
	default:
		where = append(where, repository, "folder "+folderPath)
	}
	if mb.StackLabel != "" {
		where = append(where, fmt.Sprintf("stack %q", mb.StackLabel))
	}
	what := "this change will show as drift there"
	if verb == noticeForClone {
		what = "a change made from this copy will show as drift there"
	}
	return fmt.Sprintf("managed in git (%s) — %s; `%s pull` in that repository absorbs it.",
		strings.Join(where, ", "), what, kind.Command)
}

// noteManagedBy records the notice for one row this command read, once per
// row: a push and the publish beside it may read the same row twice.
func (f *folder) noteManagedBy(claimKind, resourceID string, mb *api.ManagedBy) {
	n := noticeManagedBy(f, f.Kind, claimKind, resourceID, mb, noticeForChange)
	if n == nil {
		return
	}
	for _, have := range f.notices {
		if have.Kind == n.Kind && have.ResourceID == n.ResourceID {
			return
		}
	}
	f.notices = append(f.notices, *n)
}

// noteTableManagedBy is noteManagedBy for a pipeline row, which is a table or
// a metric by its own kind.
func (f *folder) noteTableManagedBy(row *api.Table) {
	if row == nil {
		return
	}
	kind := wfdir.ManagedTable
	if row.Kind == api.TableKindMetric {
		kind = wfdir.ManagedMetric
	}
	f.noteManagedBy(kind, row.IdentityID(), row.ManagedBy)
}

// noteWrittenAutomationsManagedBy reads `managedBy` for the automations a
// single-folder push UPDATED. The push compares against the feature's listing,
// which carries no `managedBy`, so this is the one notice that costs a request:
// one GET per row written, after the write, and never an error — a GET that
// fails says nothing, exactly as an older server does.
//
// An ADOPTED folder sends none: every row it writes is claimed at its own
// address (a row claimed elsewhere is refused before anything is written), so
// it is the folder the notice would name.
func noteWrittenAutomationsManagedBy(ctx context.Context, f *folder, result *automationPushResult) {
	if result == nil || f.managed != nil {
		return
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	for _, file := range result.Files {
		if file.Outcome != automationOutcomeUpdated || file.AutomationID == "" {
			continue
		}
		row, err := client.GetAutomation(ctx, file.AutomationID)
		if err != nil || row == nil {
			continue
		}
		f.noteManagedBy(wfdir.ManagedAutomation, row.ID, row.ManagedBy)
	}
}

// appendManagedNotice adds one row's notice, when it has one.
func appendManagedNotice(dst []managedNotice, n *managedNotice) []managedNotice {
	if n == nil {
		return dst
	}
	return append(dst, *n)
}

// printManagedNotices writes the notices to stderr, one line per distinct
// sentence: a pipeline push touching ten tables one folder manages says it
// once.
func printManagedNotices(notices []managedNotice) {
	seen := map[string]bool{}
	for _, n := range notices {
		if seen[n.Notice] {
			continue
		}
		seen[n.Notice] = true
		fmt.Fprintf(os.Stderr, "  Note: %s\n", n.Notice)
	}
}

// withManagedInGit adds the notices to a --json payload built as a map, under
// the key the result structs use — and, like theirs, only when there are some.
func withManagedInGit(payload map[string]any, notices []managedNotice) map[string]any {
	if len(notices) > 0 {
		payload["managedInGit"] = notices
	}
	return payload
}
