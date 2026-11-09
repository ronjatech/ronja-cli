package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// WHAT THIS FOLDER ITSELF PROPOSED IS NEVER DRIFT. Every test here stages the
// same history: the folder deployed directly, so each workflow, data app and
// module has a local baseline; it then proposed a change list, and the person
// approved it, which deployed a COPY of the folder. The folder's baselines
// still describe the server before the list.

// ownFolders are the proposeFixture folders whose drift guard compares a local
// baseline, and the file each one changes.
var ownFolders = []struct{ path, file string }{
	{"workflows/sales-report", "main.py"},
	{"apps/sales-dashboard", "App.tsx"},
	{"modules/sales_helpers", "money.py"},
}

// editOwnFolders changes the same file of each folder, and retitles the
// workflow.
func editOwnFolders(t *testing.T, dir, tag string) {
	t.Helper()
	for _, o := range ownFolders {
		root := filepath.Join(dir, o.path)
		writeLocal(t, root, o.file, readFile(t, root, o.file)+"\n# "+tag+"\n")
		if o.path != "workflows/sales-report" {
			continue
		}
		m := loadAnyManifest(t, root)
		m.Title = "Sales " + tag
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
}

// unpackCopy writes an archive's files into a fresh directory.
func unpackCopy(t *testing.T, archive []byte) string {
	t.Helper()
	copyDir := t.TempDir()
	for name, body := range archiveFiles(t, archive) {
		path := filepath.Join(copyDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return copyDir
}

// deployCopy unpacks a change list's copy into a fresh directory and applies
// it, as the deploy job does once the person approves. It answers the copy.
func deployCopy(t *testing.T, archive []byte) string {
	t.Helper()
	copyDir := unpackCopy(t, archive)
	if report, err := applyStack(t, copyDir, "dev", "--allow-create"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the approved copy did not deploy: %v\n%s", err, describeApply(report))
	}
	return copyDir
}

// proposeAndApprove is the history every test here starts from.
func proposeAndApprove(t *testing.T) (*fakeImportInstance, string, string) {
	t.Helper()
	f, dir := proposeFixture(t)
	editOwnFolders(t, dir, "direct")
	if report, err := applyStack(t, dir, "dev", "--allow-create"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the direct deploy: %v\n%s", err, describeApply(report))
	}
	editOwnFolders(t, dir, "proposed")
	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	copyDir := deployCopy(t, f.changeLists.proposals[0].archive)
	return f, dir, copyDir
}

// The folder edits the same files and titles again after its list deployed:
// the next proposal plans clean, and the folder's own apply pushes over what
// the list put live, leaving nothing to write.
func TestWhatThisFolderProposedIsNotDrift(t *testing.T) {
	f, dir, _ := proposeAndApprove(t)
	for _, o := range ownFolders {
		recorded := wfdir.LoadProposals(filepath.Join(dir, o.path), wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID})
		if len(recorded) != 1 || recorded[0].ListID != "changelist-1" || recorded[0].Files[o.file] == "" {
			t.Fatalf("%s recorded %+v, want the list and the hash of %s", o.path, recorded, o.file)
		}
	}
	editOwnFolders(t, dir, "later")

	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err != nil {
		t.Errorf("the folder's own approved list was refused as drift:\n%v\n%s", err, out)
	}
	report, _ := applyStack(t, dir, "dev")
	for _, o := range ownFolders {
		if line := folderLine(t, report, o.path); line.Verdict != verdictApplied {
			t.Errorf("%s: the folder's own apply over its approved list: %s %s", o.path, line.Verdict, line.Detail)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	out, _ := runCLI(t, dir, "sync", "apply", "--stack", "dev", "--dry-run", "--json")
	var after syncApplyReport
	decodeJSONInto(t, out, &after)
	for _, line := range after.Folders {
		for _, u := range line.Units {
			if u.Action != string(applyActionUnchanged) {
				t.Errorf("%s still has %s to write after the apply: the later edits did not land", line.Path, u.Action)
			}
		}
	}
}

// The negative control: after the list deployed, another checkout of the
// folder changes the same files and titles. Nothing this folder proposed
// holds that, so its apply still refuses each of them.
func TestDriftThisFolderNeverProposedIsStillRefused(t *testing.T) {
	_, dir, copyDir := proposeAndApprove(t)
	editOwnFolders(t, copyDir, "colleague")
	if report, err := applyStack(t, copyDir, "dev"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the colleague's deploy: %v\n%s", err, describeApply(report))
	}
	editOwnFolders(t, dir, "later")

	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err == nil {
		t.Fatalf("a proposal over a colleague's deploy planned clean:\n%s", out)
	}
	report, _ := applyStack(t, dir, "dev")
	for _, o := range ownFolders {
		line := folderLine(t, report, o.path)
		if line.Verdict != verdictRefused || !strings.Contains(line.Detail, o.file) {
			t.Errorf("%s: %s %s, want refused naming %s", o.path, line.Verdict, line.Detail, o.file)
		}
	}
}

func folderLine(t *testing.T, report syncApplyReport, path string) syncApplyFolderReport {
	t.Helper()
	for _, line := range report.Folders {
		if line.Path == path {
			return line
		}
	}
	t.Fatalf("no line for %s:\n%s", path, describeApply(report))
	return syncApplyFolderReport{}
}

// The title leg alone: the list retitled the workflow, and the folder
// retitles it again without touching a file.
func TestATitleThisFolderProposedIsNotDrift(t *testing.T) {
	_, dir, _ := proposeAndApprove(t)
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	m.Title = "Sales retitled"
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	report, err := applyStack(t, dir, "dev")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("a title the folder's own list set was refused as drift: %v\n%s", err, describeApply(report))
	}
}

// The history keeps the newest proposals per instance and organization, and
// leaves every other instance's alone.
func TestRecordProposalKeepsABoundedHistory(t *testing.T) {
	root := t.TempDir()
	here := wfdir.InstanceKey{URL: "https://a.example", TenantID: "t1"}
	other := wfdir.InstanceKey{URL: "https://b.example", TenantID: "t1"}
	if err := wfdir.RecordProposal(root, wfdir.Proposal{URL: other.URL, TenantID: other.TenantID, ListID: "other"}); err != nil {
		t.Fatal(err)
	}
	for i := range wfdir.MaxProposals + 2 {
		if err := wfdir.RecordProposal(root, wfdir.Proposal{URL: here.URL, TenantID: here.TenantID, ListID: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	got := wfdir.LoadProposals(root, here)
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ListID)
	}
	if strings.Join(ids, "") != "gfedc" {
		t.Errorf("kept %v, want the newest %d, newest first", ids, wfdir.MaxProposals)
	}
	if o := wfdir.LoadProposals(root, other); len(o) != 1 || o[0].ListID != "other" {
		t.Errorf("the other instance's history: %+v", o)
	}
}

// MODULE, at the guard: the list changed a file, renamed the package and
// retitled the module, and the folder changes all three again. The negative
// controls are the same remote with a file and a title nobody here proposed,
// with the head moved since this stack's deploy, with the proposal made over
// another baseline, and with the file and the metadata held by two different
// proposals.
func TestModuleDriftExcusesWhatThisFolderProposed(t *testing.T) {
	key := moduleKey()
	baseline := &wfdir.InstanceState{SourceID: "module-draft", Name: "emab_tracker", Title: "EMAB tracker",
		Files: map[string]wfdir.FileState{
			"__init__.py": {SHA256: wfdir.HashString("")},
			"tracker.py":  {SHA256: wfdir.HashString("x = 1")},
		}}
	over := wfdir.BaselineDigest(baseline)
	proposedFiles := map[string]string{"__init__.py": wfdir.HashString(""), "tracker.py": wfdir.HashString("x = 2")}
	record := func(t *testing.T, root, listID string, files map[string]string, m *wfdir.Manifest, over string) {
		t.Helper()
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := wfdir.RecordProposal(root, wfdir.Proposal{URL: key.URL, TenantID: key.TenantID, ListID: listID,
			Files: files, Manifest: raw, Over: over}); err != nil {
			t.Fatal(err)
		}
	}
	proposedManifest := &wfdir.Manifest{Kind: wfdir.KindModule, Name: "emab_proposed", Title: "EMAB proposed"}
	newRoot := func(t *testing.T) string {
		return moduleTestFolder(t, key, wfdir.Binding{ModuleID: "module-1", FeatureID: "collection-1"}, nil)
	}
	vouches := headAgreement{Recorded: "modv-2", Current: "modv-2", AnchoredDraft: true}
	local := map[string]string{"__init__.py": "", "tracker.py": "x = 3"}
	guard := func(root string, remote map[string]string, title string, head headAgreement) (driftVerdict, error) {
		f := moduleFolderFor(t, root, key, baseline)
		f.Manifest.Name, f.Manifest.Title = "emab_later", "EMAB later"
		identity := &api.Module{ID: "module-1", Name: "emab_proposed", Title: title, Lifecycle: api.LifecycleLive}
		return checkModuleDrift(f, identity, remote, local, false, head)
	}
	asProposed := map[string]string{"__init__.py": "", "tracker.py": "x = 2"}

	root := newRoot(t)
	record(t, root, "changelist-1", proposedFiles, proposedManifest, over)
	verdict, err := guard(root, asProposed, "EMAB proposed", vouches)
	if err != nil {
		t.Fatalf("what this folder's own list put there was refused as drift: %v", err)
	}
	if verdict.BaselineClean || verdict.Preconditions["tracker.py"] != wfdir.HashString("x = 2") {
		t.Errorf("verdict = %+v, want an unclean baseline and writes armed on the listing", verdict)
	}
	if _, err := guard(root, map[string]string{"__init__.py": "", "tracker.py": "x = colleague"}, "EMAB proposed", vouches); err == nil || !strings.Contains(err.Error(), "tracker.py") {
		t.Errorf("a file nobody here proposed: %v, want a refusal naming tracker.py", err)
	}
	if _, err := guard(root, asProposed, "Colleague title", vouches); err == nil || !strings.Contains(err.Error(), "Colleague title") {
		t.Errorf("a title nobody here proposed: %v, want a refusal naming it", err)
	}
	moved := headAgreement{Recorded: "modv-2", Current: "modv-3", AnchoredDraft: true}
	if _, err := guard(root, asProposed, "EMAB proposed", moved); err == nil || !strings.Contains(err.Error(), "tracker.py") {
		t.Errorf("published to since this stack's deploy: %v, want a refusal naming tracker.py", err)
	}

	otherBaseline := newRoot(t)
	record(t, otherBaseline, "changelist-1", proposedFiles, proposedManifest, wfdir.HashString("another baseline"))
	if _, err := guard(otherBaseline, asProposed, "EMAB proposed", vouches); err == nil || !strings.Contains(err.Error(), "tracker.py") {
		t.Errorf("a proposal made over another baseline: %v, want a refusal naming tracker.py", err)
	}

	mixed := newRoot(t)
	record(t, mixed, "changelist-1", proposedFiles, &wfdir.Manifest{Kind: wfdir.KindModule, Name: "emab_tracker", Title: "EMAB tracker"}, over)
	record(t, mixed, "changelist-2", map[string]string{"__init__.py": wfdir.HashString(""), "tracker.py": wfdir.HashString("x = 1")}, proposedManifest, over)
	if _, err := guard(mixed, asProposed, "EMAB proposed", vouches); err == nil || !strings.Contains(err.Error(), "tracker.py") {
		t.Errorf("one list holds the file and another the metadata: %v, want a refusal", err)
	}
}

// A proposed.json that does not parse is no history: it excuses nothing, and
// the next proposal replaces it.
func TestACorruptProposalHistoryExcusesNothing(t *testing.T) {
	root := t.TempDir()
	key := wfdir.InstanceKey{URL: "https://a.example", TenantID: "t1"}
	if err := os.MkdirAll(filepath.Join(root, wfdir.StateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, ".ronja/"+wfdir.ProposedFileName, `{"proposals": [{"url": `)
	if got := wfdir.LoadProposals(root, key); got != nil {
		t.Fatalf("a corrupt history loaded %+v, want none", got)
	}
	if err := wfdir.RecordProposal(root, wfdir.Proposal{URL: key.URL, TenantID: key.TenantID, ListID: "changelist-1"}); err != nil {
		t.Fatal(err)
	}
	if got := wfdir.LoadProposals(root, key); len(got) != 1 || got[0].ListID != "changelist-1" {
		t.Fatalf("after a new proposal: %+v, want only it", got)
	}
}

// colleagueCheckout is a colleague's git checkout of dir as it stands: every
// folder's committed files, and no .ronja/.
func colleagueCheckout(t *testing.T, dir string) string {
	t.Helper()
	found, err := discoverFolders(dir)
	if err != nil {
		t.Fatal(err)
	}
	archive, _, err := packProposal(dir, found, nil)
	if err != nil {
		t.Fatal(err)
	}
	return unpackCopy(t, archive)
}

// proposeAndDeploy proposes dir and deploys the list's copy, as the person's
// approval does.
func proposeAndDeploy(t *testing.T, f *fakeImportInstance, dir string) {
	t.Helper()
	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	deployCopy(t, f.changeLists.proposals[len(f.changeLists.proposals)-1].archive)
}

// applied deploys dir directly and fails the test unless it applied.
func applied(t *testing.T, dir, what string) {
	t.Helper()
	if report, err := applyStack(t, dir, "dev"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("%s: %v\n%s", what, err, describeApply(report))
	}
}

const salesReport = "workflows/sales-report"

func editLine(t *testing.T, root, file, line string) {
	t.Helper()
	writeLocal(t, root, file, readFile(t, root, file)+"\n# "+line+"\n")
}

// B1, the propose-only flow, where the baseline never moves. L1 deletes a
// file, L2 restores it, and a colleague deletes it again and deploys. L1
// holds the deletion and L2 the rest of what is live, but no ONE list holds
// both, so the agent's next proposal is refused naming the file.
func TestAnOlderListNeverExcusesAColleaguesDeletion(t *testing.T) {
	f, dir := proposeFixture(t)
	wf := filepath.Join(dir, salesReport)
	writeLocal(t, wf, "extra.py", "X = 1\n")
	if report, err := applyStack(t, dir, "dev", "--allow-create"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the direct deploy: %v\n%s", err, describeApply(report))
	}

	if err := os.Remove(filepath.Join(wf, "extra.py")); err != nil {
		t.Fatal(err)
	}
	editLine(t, wf, "main.py", "l1")
	proposeAndDeploy(t, f, dir)

	writeLocal(t, wf, "extra.py", "X = 1\n")
	editLine(t, wf, "main.py", "l2")
	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err != nil {
		t.Fatalf("propose L2: %v\n%s", err, out)
	}
	l2 := deployCopy(t, f.changeLists.proposals[len(f.changeLists.proposals)-1].archive)

	if err := os.Remove(filepath.Join(l2, salesReport, "extra.py")); err != nil {
		t.Fatal(err)
	}
	applied(t, l2, "the colleague's deploy")

	editLine(t, wf, "main.py", "l3")
	out, err := runPropose_(t, dir, "--stack", "dev", "--json")
	if err == nil || !strings.Contains(err.Error(), "extra.py") {
		t.Fatalf("the agent's L3 over a colleague's deletion: %v\n%s, want a refusal naming extra.py", err, out)
	}
}

// The head-moved control: L1 and L2 deploy, then a colleague rolls the
// workflow back to L1's bytes in Ronja. Live now holds exactly what L1 held,
// over the same baseline, and only the head can tell: it moved past the
// version this stack deployed.
func TestAListCountsOnlyWhileTheHeadIsThisStacksDeploy(t *testing.T) {
	f, dir, l1 := proposeAndApprove(t)
	wf := filepath.Join(dir, salesReport)
	editLine(t, wf, "main.py", "l2")
	proposeAndDeploy(t, f, dir)

	lock, err := wfdir.LoadLock(wf)
	if err != nil {
		t.Fatal(err)
	}
	id := lock.Stacks["dev"].WorkflowID
	client := api.New(f.URL(), "test-token")
	ctx := t.Context()
	draft, err := client.CheckoutWorkflow(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutWorkflowFile(ctx, draft.ID, "main.py", readFile(t, l1, salesReport, "main.py"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CommitWorkflowDraft(ctx, draft.ID, "", ""); err != nil {
		t.Fatal(err)
	}

	editLine(t, wf, "main.py", "l3")
	out, err := runPropose_(t, dir, "--stack", "dev", "--json")
	if err == nil || !strings.Contains(err.Error(), "main.py") {
		t.Fatalf("a rollback published in Ronja to L1's bytes: %v\n%s, want a refusal naming main.py", err, out)
	}
}

// Every proposal records the baseline the guard held, read the same way the
// guard reads it: two proposals over one baseline both count, and the push
// that records a new baseline retires both.
func TestAListIsOverTheBaselineTheGuardHolds(t *testing.T) {
	f, dir, _ := proposeAndApprove(t)
	editOwnFolders(t, dir, "later")
	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err != nil {
		t.Fatalf("the second proposal: %v\n%s", err, out)
	}
	key := wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID}
	digest := func(root string) string {
		t.Helper()
		state, err := wfdir.LoadState(root)
		if err != nil {
			t.Fatal(err)
		}
		return wfdir.BaselineDigest(state.For(wfdir.BaselineKey{Key: key, Stack: "dev"}))
	}
	before := map[string]string{}
	for _, o := range ownFolders {
		root := filepath.Join(dir, o.path)
		before[o.path] = digest(root)
		recorded := wfdir.LoadProposals(root, key)
		if len(recorded) != 2 || before[o.path] == "" {
			t.Fatalf("%s recorded %d proposals over %q, want 2 over its baseline", o.path, len(recorded), before[o.path])
		}
		for _, p := range recorded {
			if p.Over != before[o.path] {
				t.Errorf("%s: %s is over %s, want the baseline %s", o.path, p.ListID, p.Over, before[o.path])
			}
		}
	}
	applied(t, dir, "the folder's own apply over its approved list")
	for _, o := range ownFolders {
		if after := digest(filepath.Join(dir, o.path)); after == before[o.path] {
			t.Errorf("%s: the apply recorded no new baseline, so its proposals still count", o.path)
		}
	}
}

// The code review's case: L1 never held extra.py. The folder then pushes it
// directly, which records a new baseline, and a colleague deletes it from a
// fresh checkout. L1 still holds what live holds, and the head is the
// colleague's deploy, but L1 was proposed over a baseline this folder has
// moved past.
func TestAListOverAnOlderBaselineNeverExcusesADeletion(t *testing.T) {
	_, dir, _ := proposeAndApprove(t)
	wf := filepath.Join(dir, salesReport)
	writeLocal(t, wf, "extra.py", "X = 1\n")
	applied(t, dir, "the folder's direct push of extra.py")

	colleague := colleagueCheckout(t, dir)
	if err := os.Remove(filepath.Join(colleague, salesReport, "extra.py")); err != nil {
		t.Fatal(err)
	}
	applied(t, colleague, "the colleague's deploy")

	editLine(t, wf, "main.py", "later")
	report, _ := applyStack(t, dir, "dev")
	if line := folderLine(t, report, salesReport); line.Verdict != verdictRefused || !strings.Contains(line.Detail, "extra.py") {
		t.Errorf("%s: %s %s, want refused naming extra.py", salesReport, line.Verdict, line.Detail)
	}
}

// The rollback: L1 deploys, the folder pushes a later main.py directly, and
// a colleague rolls it back to L1's bytes from a fresh checkout.
func TestAListOverAnOlderBaselineNeverExcusesARollback(t *testing.T) {
	_, dir, l1 := proposeAndApprove(t)
	wf := filepath.Join(dir, salesReport)
	editLine(t, wf, "main.py", "direct")
	applied(t, dir, "the folder's direct push")

	colleague := colleagueCheckout(t, dir)
	writeLocal(t, filepath.Join(colleague, salesReport), "main.py", readFile(t, l1, salesReport, "main.py"))
	applied(t, colleague, "the colleague's rollback")

	editLine(t, wf, "main.py", "later")
	report, _ := applyStack(t, dir, "dev")
	if line := folderLine(t, report, salesReport); line.Verdict != verdictRefused || !strings.Contains(line.Detail, "main.py") {
		t.Errorf("%s: %s %s, want refused naming main.py", salesReport, line.Verdict, line.Detail)
	}
}
