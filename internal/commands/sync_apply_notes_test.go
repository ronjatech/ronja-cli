package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A folder deployed unchecked beside adopted ones is said ACTION FIRST: the
// first line says what happened and names the command that undoes it, and
// the why follows on a line of its own.
func TestUnmanagedFolderNotesLeadWithTheCommand(t *testing.T) {
	adopted := &folder{Stack: "main", Binding: wfdir.Binding{FeatureID: "collection-1"},
		Manifest: &wfdir.Manifest{ID: "folder-1", Stacks: map[string]wfdir.Stack{"main": {FeatureID: "collection-1"}}}}
	unmanaged := &folder{Stack: "main", Binding: wfdir.Binding{FeatureID: "collection-1"},
		Manifest: &wfdir.Manifest{ID: "folder-2", Stacks: map[string]wfdir.Stack{"main": {FeatureID: "collection-1", Unmanaged: true}}}}
	classic := &folder{Stack: "main", Binding: wfdir.Binding{FeatureID: "collection-1"},
		Manifest: &wfdir.Manifest{Stacks: map[string]wfdir.Stack{"main": {FeatureID: "collection-1"}}}}
	planned := []plannedFolder{{f: adopted}, {f: unmanaged}, {f: classic}}
	noteUnmanagedFolders(planned)

	if len(planned[0].line.Notes) != 0 {
		t.Errorf("an adopted folder got a note: %v", planned[0].line.Notes)
	}
	for i, want := range map[int]string{
		1: "`ronja sync adopt --stack main` turns the check back on.",
		2: "`ronja sync adopt --stack main` adopts it.",
	} {
		notes := planned[i].line.Notes
		if len(notes) != 1 {
			t.Fatalf("folder %d notes = %v, want one", i, notes)
		}
		first, why, ok := strings.Cut(notes[0], "\n")
		if !ok || !strings.HasSuffix(first, want) {
			t.Errorf("folder %d: the note's first line does not end with the command: %q", i, first)
		}
		if !strings.Contains(why, "not checked against the stack's feature") || !strings.Contains(why, "keeps no record") {
			t.Errorf("folder %d: the explanation lost a fact: %q", i, why)
		}
	}
}

// The human report indents every line of a warning or a note after its first,
// so the action line stands out and the why reads as belonging to it.
func TestApplyReportIndentsTheExplanation(t *testing.T) {
	report := &syncApplyReport{
		Dir: "/tree", URL: "https://ronja.example", Order: applyOrderDependency, Verdict: verdictApplied,
		Warnings: []syncApplyWarning{{Reason: syncWarningSharedModule, Path: "modules/m", Detail: "Do this.\nBecause of that."}},
		Folders: []syncApplyFolderReport{{Path: "pipelines/p", Kind: wfdir.KindPipeline, Verdict: verdictApplied,
			Units: []syncApplyUnitReport{}, Notes: []string{"Check this.\nIt was admitted."}}},
		Summary: syncApplySummary{Folders: 1, Applied: 1},
	}
	stdout, restore := captureStdout(t)
	printSyncApply(report)
	out := restore(stdout)
	for _, want := range []string{
		"\n    Do this.\n      Because of that.\n",
		"note       Check this.\n                          It was admitted.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not indent the explanation (%q):\n%s", want, out)
		}
	}
}
