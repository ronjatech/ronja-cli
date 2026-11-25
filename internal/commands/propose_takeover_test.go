package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// takeoverFixture is setupFixture with the app already proposed once from a
// folder of its own, as a chat's first computer leaves it: folder "first"
// holds data_app-kpi, and its list is still pending.
func takeoverFixture(t *testing.T) (*fakeImportInstance, string, string) {
	t.Helper()
	f, repo := setupFixture(t)
	first := cloneKPI(t, repo, "first", "--stack", "main")
	writeLocal(t, first, "App.tsx", "export default function App() { return 'first' }\n")
	if out, err := runPropose_(t, first); err != nil {
		t.Fatalf("propose from the first folder: %v\n%s", err, out)
	}
	if loadAnyManifest(t, first).ID == "" || kpiHolder(f) != loadAnyManifest(t, first).ID {
		t.Fatalf("the first folder does not hold the app: holder %q", kpiHolder(f))
	}
	return f, repo, first
}

func cloneKPI(t *testing.T, repo, name string, args ...string) string {
	t.Helper()
	if out, err := runCLI(t, repo, append([]string{"app", "clone", "data_app-kpi", name}, args...)...); err != nil {
		t.Fatalf("clone %s: %v\n%s", name, err, out)
	}
	return filepath.Join(repo, name)
}

// kpiHolder is the folder id of data_app-kpi's live claim, "" for none.
func kpiHolder(f *fakeImportInstance) string {
	for _, c := range f.managed.Claims(importFeature) {
		if c.ResourceID == "data_app-kpi" && !c.departed() {
			return c.FolderID
		}
	}
	return ""
}

// A fresh clone of a held app proposes with --take-over: the other folder's
// claim is released, the app is claimed for this one, and the report names the
// folder it was taken from. Without the flag the same propose is refused
// claimed_elsewhere, naming the holder, its source label and the flag.
func TestProposeTakeOverContinuesAnAppAnotherFolderHolds(t *testing.T) {
	f, repo, first := takeoverFixture(t)
	firstID := loadAnyManifest(t, first).ID
	second := cloneKPI(t, repo, "second", "--stack", "main")
	writeLocal(t, second, "App.tsx", "export default function App() { return 'second' }\n")

	_, err := runPropose_(t, second)
	if err == nil {
		t.Fatal("propose of a clone whose app another folder holds succeeded without --take-over")
	}
	for _, want := range []string{
		"(claimed_elsewhere)",
		"folder " + firstID,
		"first" + localSourceSuffix,
		"propose again with --take-over",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "claim_refused") {
		t.Errorf("the refusal is the folder-level claim_refused:\n%v", err)
	}
	if kpiHolder(f) != firstID || len(f.changeLists.proposals) != 1 {
		t.Fatalf("the refused propose moved the claim (%q) or proposed (%d lists)", kpiHolder(f), len(f.changeLists.proposals))
	}

	// Run again with the flag: the folder kept the id the refused run wrote.
	out, err := runPropose_(t, second, "--take-over", "--json")
	if err != nil {
		t.Fatalf("propose --take-over: %v\n%s", err, out)
	}
	var report proposeReport
	decodeJSONInto(t, out, &report)
	secondID := loadAnyManifest(t, second).ID
	if secondID == "" || secondID == firstID || kpiHolder(f) != secondID {
		t.Fatalf("after the takeover the app is held by %q; first %q, second %q", kpiHolder(f), firstID, secondID)
	}
	if report.Setup == nil || len(report.Setup.TookOverFrom) != 1 || !strings.Contains(report.Setup.TookOverFrom[0], firstID) {
		t.Fatalf("the report does not name the folder taken over from: %+v", report.Setup)
	}
	if report.Status != changeListUnsent || report.Counts.Modified != 1 || len(f.changeLists.proposals) != 2 {
		t.Fatalf("report = %+v, %d proposals", report, len(f.changeLists.proposals))
	}

	// The displaced folder's next propose is refused, naming the new holder.
	_, err = runPropose_(t, first)
	if err == nil || !strings.Contains(err.Error(), "claimed_elsewhere") || !strings.Contains(err.Error(), secondID) {
		t.Fatalf("the displaced folder's propose was not refused claimed_elsewhere naming %s: %v", secondID, err)
	}
	if len(f.changeLists.proposals) != 2 {
		t.Fatal("the displaced folder proposed a list")
	}

	// Taking over again from the holder itself is a no-op.
	if out, err := runPropose_(t, second, "--take-over", "--json"); err != nil {
		t.Fatalf("a second propose --take-over from the holder: %v\n%s", err, out)
	} else {
		var again proposeReport
		decodeJSONInto(t, out, &again)
		if again.Setup != nil || kpiHolder(f) != secondID {
			t.Fatalf("a second propose --take-over took over again: %+v, holder %q", again.Setup, kpiHolder(f))
		}
	}
}

// A folder that records no live version it is based on is refused before
// anything is written, naming the clone that records one.
func TestProposeTakeOverRefusesAFolderWithNoBase(t *testing.T) {
	f, repo, first := takeoverFixture(t)
	second := cloneKPI(t, repo, "second")
	before := readManifestBytes(t, second)
	f.resetRequests()

	_, err := runPropose_(t, second, "--take-over")
	if err == nil {
		t.Fatal("propose --take-over of a clone with no recorded version succeeded")
	}
	if !strings.Contains(err.Error(), "`ronja app clone data_app-kpi <dir> --stack main`") {
		t.Fatalf("the refusal does not name the clone that records a version: %v", err)
	}
	if after := readManifestBytes(t, second); after != before || len(f.writes()) != 0 {
		t.Fatalf("the refused takeover wrote: ronja.json changed %v, requests %v", after != before, f.writes())
	}
	if kpiHolder(f) != loadAnyManifest(t, first).ID {
		t.Fatal("the claim moved")
	}
}

// A row a git repository's folder holds is never taken over; the refusal
// names the repository and how to edit it there.
func TestProposeTakeOverRefusesARowHeldInGit(t *testing.T) {
	f, repo := setupFixture(t)
	repoFolder := "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08"
	f.managed.stage(importFeature, repoFolder, "", "dataapp", "data_app-kpi", "github.com/acme/data//apps/kpi")
	second := cloneKPI(t, repo, "second", "--stack", "main")

	_, err := runPropose_(t, second, "--take-over")
	if err == nil {
		t.Fatal("a takeover of a row a git repository holds succeeded")
	}
	for _, want := range []string{"(claimed_elsewhere)", "github.com/acme/data//apps/kpi", "in a git repository", "`ronja app pull`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if kpiHolder(f) != repoFolder || len(f.changeLists.proposals) != 0 {
		t.Fatalf("the repository's folder lost the row (%q) or a list was proposed", kpiHolder(f))
	}
}

// A row whose holder recorded no source label (only a raw claim sends none)
// is never taken over: nothing says the holder is not a repository's folder.
// The refusal says who can release it.
func TestProposeTakeOverRefusesAHolderWithNoSource(t *testing.T) {
	f, repo := setupFixture(t)
	rawFolder := "5d2a9e41-7c3b-4f8a-b1e6-0c9d8f7a6b52"
	f.managed.stage(importFeature, rawFolder, "", "dataapp", "data_app-kpi", "")
	second := cloneKPI(t, repo, "second", "--stack", "main")

	_, err := runPropose_(t, second, "--take-over")
	if err == nil {
		t.Fatal("a takeover of a row whose holder recorded no source succeeded")
	}
	for _, want := range []string{"(claimed_elsewhere)", "folder " + rawFolder, "source is unknown", "`ronja sync unmanage --force` in that folder"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if kpiHolder(f) != rawFolder || len(f.changeLists.proposals) != 0 {
		t.Fatalf("the holder lost the row (%q) or a list was proposed", kpiHolder(f))
	}
}

// A clone taken before someone published is not based on what is live: the
// takeover is refused, and the holder keeps the row.
func TestProposeTakeOverRefusesAStaleFolder(t *testing.T) {
	f, repo, first := takeoverFixture(t)
	second := cloneKPI(t, repo, "second", "--stack", "main")
	f.app.AddDraft("data_app-kpi", "data_app-kpi-draft", api.DataAppFile{Path: "App.tsx", Content: "export default function App() { return 'web' }\n"})
	f.app.commitDraft("data_app-kpi-draft")

	_, err := runPropose_(t, second, "--take-over")
	if err == nil {
		t.Fatal("a takeover from a folder cloned before a publish succeeded")
	}
	for _, want := range []string{"(claimed_elsewhere)", "is based on version dav-kpi-1", "Clone the live data app again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if kpiHolder(f) != loadAnyManifest(t, first).ID {
		t.Fatal("the claim moved")
	}
}

// A clone next to the original in one tree gets its own folder id, so the
// tree holds two folders with two ids and no duplicate_folder_id; after the
// takeover a dry run of the tree refuses the original claimed_elsewhere.
func TestProposeTakeOverCloneBesideTheOriginalGetsItsOwnID(t *testing.T) {
	f, repo, first := takeoverFixture(t)
	second := cloneKPI(t, repo, "second", "--stack", "main")
	writeLocal(t, second, "App.tsx", "export default function App() { return 'second' }\n")
	if out, err := runPropose_(t, second, "--take-over"); err != nil {
		t.Fatalf("propose --take-over: %v\n%s", err, out)
	}
	firstID, secondID := loadAnyManifest(t, first).ID, loadAnyManifest(t, second).ID
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("folder ids: first %q, second %q", firstID, secondID)
	}
	out, _ := runCLI(t, repo, "sync", "apply", "--json", "--allow-create", "--dry-run", "--stack", "main")
	var report syncApplyReport
	decodeJSONInto(t, out, &report)
	reasons := map[string]string{}
	for _, folder := range report.Folders {
		reasons[folder.Path] = folder.Reason
		if folder.Reason == syncReasonDuplicateFolderID {
			t.Fatalf("the tree reads a duplicate folder id:\n%s", out)
		}
	}
	if reasons["first"] != syncReasonClaimedElsewhere || reasons["second"] != "" {
		t.Fatalf("dry run reasons %v, want first claimed_elsewhere and second clean:\n%s", reasons, out)
	}
	if kpiHolder(f) != secondID {
		t.Fatal("the takeover did not hold")
	}
}

// Two checkouts, two pending change lists: the first folder's list, proposed
// before the second folder took the app over, and the second's. Whichever is
// approved first, the job (`sync apply` on the uploaded copy, as
// echangelist.Runner runs it) deploys the holder's list and refuses the
// displaced folder's claimed_elsewhere before it writes anything, so live
// holds the holder's content.
func TestTakenOverFolderPendingListIsRefused(t *testing.T) {
	for _, order := range []string{"holder first", "displaced first"} {
		t.Run(order, func(t *testing.T) {
			f, repo, _ := takeoverFixture(t)
			second := cloneKPI(t, repo, "second", "--stack", "main")
			writeLocal(t, second, "App.tsx", "export default function App() { return 'second' }\n")
			if out, err := runPropose_(t, second, "--take-over"); err != nil {
				t.Fatalf("propose --take-over: %v\n%s", err, out)
			}
			if n := len(f.changeLists.proposals); n != 2 {
				t.Fatalf("%d lists pending, want 2", n)
			}
			displaced := unpackProposal(t, f.changeLists.proposals[0])
			holder := unpackProposal(t, f.changeLists.proposals[1])

			runJob := func(copyDir string) (syncApplyReport, error) {
				t.Helper()
				if out, err := runCLI(t, t.TempDir(), "sync", "apply", "--json", "--allow-create", "--dir", copyDir, "--stack=main", "--dry-run"); err != nil {
					var replan syncApplyReport
					decodeJSONInto(t, out, &replan)
					return replan, err
				}
				out, err := runCLI(t, t.TempDir(), "sync", "apply", "--json", "--allow-create", "--dir", copyDir, "--stack=main")
				var applied syncApplyReport
				decodeJSONInto(t, out, &applied)
				return applied, err
			}
			first, second2 := holder, displaced
			if order == "displaced first" {
				first, second2 = displaced, holder
			}
			for _, copyDir := range []string{first, second2} {
				report, err := runJob(copyDir)
				if copyDir == holder {
					if err != nil {
						t.Fatalf("the holder's list did not deploy: %v\n%+v", err, report)
					}
					continue
				}
				if err == nil || len(report.Folders) != 1 || report.Folders[0].Reason != syncReasonClaimedElsewhere {
					t.Fatalf("the displaced folder's list was not refused claimed_elsewhere: %v\n%+v", err, report)
				}
			}
			if live := f.app.FileContents("data_app-kpi")["App.tsx"]; !strings.Contains(live, "'second'") {
				t.Fatalf("live is not the holder's content: %s", live)
			}
		})
	}
}

// unpackProposal writes a proposal's copy into a fresh directory, as the job
// unpacks it, with a home of its own.
func unpackProposal(t *testing.T, p fakeProposal) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range archiveFiles(t, p.archive) {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, wfdir.ManifestName)); err != nil {
		t.Fatalf("the copy holds no %s: %v", wfdir.ManifestName, err)
	}
	return dir
}

// A clone of a row a folder outside git holds says how to continue it from
// the copy; a repository's row keeps the pull notice.
func TestCloneNoticeNamesTakeOverForAFolderOutsideGit(t *testing.T) {
	local := &api.ManagedBy{FolderID: "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08", SourceLabel: "work/apps/kpi" + localSourceSuffix, StackLabel: "main"}
	if got := describeManagedBy(wfdir.DataAppKind, local, noticeForClone); !strings.Contains(got, "`ronja propose --take-over`") || !strings.Contains(got, "clone it with --stack") {
		t.Errorf("the clone notice for a folder outside git = %q", got)
	}
	if got := describeManagedBy(wfdir.DataAppKind, local, noticeForChange); strings.Contains(got, "--take-over") {
		t.Errorf("a change notice names --take-over: %q", got)
	}
	repo := &api.ManagedBy{FolderID: local.FolderID, SourceLabel: "github.com/acme/data//apps/kpi", StackLabel: "main"}
	if got := describeManagedBy(wfdir.DataAppKind, repo, noticeForClone); strings.Contains(got, "--take-over") || !strings.Contains(got, "`ronja app pull`") {
		t.Errorf("the clone notice for a repository's folder = %q", got)
	}
}

// --take-over on a lone folder that cannot be read names that folder and why,
// not a count of folders.
func TestProposeTakeOverNamesAnUnreadableFolder(t *testing.T) {
	_, repo, _ := takeoverFixture(t)
	second := cloneKPI(t, repo, "second", "--stack", "main")
	writeLocal(t, second, "ronja.json", "{not json")

	_, err := runPropose_(t, second, "--take-over")
	if err == nil {
		t.Fatal("propose --take-over of an unreadable folder succeeded")
	}
	if !strings.Contains(err.Error(), "could not be read") || !strings.Contains(err.Error(), second) || strings.Contains(err.Error(), "holds 1") {
		t.Fatalf("the refusal does not name the unreadable folder and why: %v", err)
	}
}

// A displaced folder's dry run that cannot read the feature's claims says so
// on stderr: its plan may hold rows another folder holds, which the apply
// refuses.
func TestDisplacedFolderDryRunWarnsWhenTheClaimsCannotBeRead(t *testing.T) {
	f, repo, first := takeoverFixture(t)
	second := cloneKPI(t, repo, "second", "--stack", "main")
	if out, err := runPropose_(t, second, "--take-over"); err != nil {
		t.Fatalf("propose --take-over: %v\n%s", err, out)
	}
	f.managed.failFeatureList = true
	stderr := captureStderr(t, func() {
		_, _ = runCLI(t, first, "sync", "apply", "--dry-run", "--stack", "main")
	})
	if !strings.Contains(stderr, "could not read feature") || !strings.Contains(stderr, "the apply will refuse them") {
		t.Fatalf("stderr does not warn that the claims could not be read:\n%s", stderr)
	}
}
