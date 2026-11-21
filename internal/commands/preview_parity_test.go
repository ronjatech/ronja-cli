package commands

import (
	"cmp"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// THE PREVIEW REFUSES WHAT THE DEPLOY REFUSES. A change list deploys a copy of
// its folders, which never holds .ronja/, so `sync apply --dry-run` on such a
// copy, and `ronja propose`, ask the push's own overwrite check and title rule
// exactly as the copy's `sync apply` will.

// openOwnDraft opens the caller's own draft of a draft-kind fixture's live
// row, holding content in its main file, and answers its id.
func openOwnDraft(fx idempotenceFixture, content string) string {
	switch {
	case fx.wf != nil:
		return fx.wf.AddDraft("wf-1", "draft-own", api.WorkflowFile{Path: "main.py", Content: content},
			api.WorkflowFile{Path: "helper.py", Content: "def main():\n    pass\n"}).ID
	case fx.app != nil:
		return fx.app.AddDraft("app-1", "data_app-draft-own", api.DataAppFile{Path: "App.tsx", Content: content}).ID
	default:
		return openModuleDraft(fx.mod, "module-1", "module-draft-own", fakeCallerUserID, fakeCallerUserID, "tracker.py", content)
	}
}

// openModuleDraft opens the one draft of a live module, drafted by drafter and
// last edited by editor.
func openModuleDraft(f *fakeInstance, liveID, draftID, drafter, editor, path, content string) string {
	live := f.mod.rows[liveID]
	f.AddModule(&api.Module{
		ID: draftID, ParentModuleID: liveID, Lifecycle: api.LifecycleDraft, FeatureID: live.FeatureID, Name: live.Name, Title: live.Title,
		DrafterUserID: drafter, UpdatedBy: editor, BaseVersionID: f.moduleHeadVersionID(liveID), UpdatedAt: live.UpdatedAt.Add(time.Hour),
	}, api.ModuleFile{Path: moduleInitFile, Content: fakeModuleInitSeed}, api.ModuleFile{Path: path, Content: content})
	f.mod.draftOf[liveID] = draftID
	return draftID
}

// setAppliedTitle makes a folder's stack record title as its applied title,
// in its lock and on its claim; "" records none.
func setAppliedTitle(t *testing.T, m *fakeManaged, root, stack, title string) {
	t.Helper()
	editLock(t, root, func(l *wfdir.Lock) { l.SetAppliedTitle(stack, title) })
	manifest := loadAnyManifest(t, root)
	c := m.ClaimAt(manifest.Stacks[stack].FeatureID, manifest.ID, "")
	if c == nil {
		t.Fatalf("%s has no claim on stack %s", root, stack)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if title == "" {
		delete(c.Applied, "title")
	} else {
		c.Applied["title"] = title
	}
}

// previewLine is `sync apply --dry-run`'s line for a fixture's folder.
func previewLine(t *testing.T, fx idempotenceFixture) syncApplyFolderReport {
	t.Helper()
	out, _ := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	return applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
}

func TestDryRunAgreesWithApply(t *testing.T) {
	type row struct {
		kinds []string
		stage func(t *testing.T, fx idempotenceFixture)
		// refused: what `sync apply` answers, so a row both get wrong the same
		// way still fails.
		refused bool
		// reason, when set, is the refusal both must name.
		reason string
	}
	all := []string{"workflow", "data app", "module"}
	asCopy := func(t *testing.T, fx idempotenceFixture) { dropLocalState(t, fx.root) }
	rows := map[string]row{
		"copy: no recorded version": {all, func(t *testing.T, fx idempotenceFixture) {
			forgetHeadVersion(t, managedFakeOf(fx), fx.root, "prod")
			fx.edit(t)
			asCopy(t, fx)
		}, true, ""},
		"copy: recorded version moved": {all, func(t *testing.T, fx idempotenceFixture) {
			fx.draftKind.webPublish("colleague\n")
			fx.edit(t)
			asCopy(t, fx)
		}, true, ""},
		"copy: own draft open": {all, func(t *testing.T, fx idempotenceFixture) {
			openOwnDraft(fx, "work in progress\n")
			fx.edit(t)
			asCopy(t, fx)
		}, true, syncReasonDraftOpen},
		// With the draft open, what the push would refuse once it is discarded
		// is the refusal: a deploy that discards it would then deploy nothing.
		"copy: own draft open, app retitled elsewhere": {[]string{"data app"}, func(t *testing.T, fx idempotenceFixture) {
			fx.app.apps["app-1"].Name = "Dashboard"
			setAppTitle(t, fx.root, "Dashboard")
			setAppliedTitle(t, managedFakeOf(fx), fx.root, "prod", "Dashboard")
			fx.app.apps["app-1"].Name = "Renamed in the web app"
			openOwnDraft(fx, "work in progress\n")
			fx.edit(t)
			asCopy(t, fx)
		}, true, syncReasonNotDeployed},
		"copy: own draft open, workflow renamed by the folder, no applied title recorded": {[]string{"workflow"}, func(t *testing.T, fx idempotenceFixture) {
			setAppliedTitle(t, managedFakeOf(fx), fx.root, "prod", "")
			fx.draftKind.retitle(t, "Quarterly report")
			openOwnDraft(fx, "work in progress\n")
			asCopy(t, fx)
		}, true, syncReasonNotDeployed},
		"copy: own draft open, runtime lowered": {[]string{"workflow"}, func(t *testing.T, fx idempotenceFixture) {
			fx.wf.workflows["wf-1"].RuntimeVersion = wfdir.RuntimeQuery
			setManifestRuntime(t, fx.root, wfdir.RuntimeDefault)
			openOwnDraft(fx, "work in progress\n")
			fx.edit(t)
			asCopy(t, fx)
		}, true, syncReasonNotDeployed},
		"copy: runtime lowered": {[]string{"workflow"}, func(t *testing.T, fx idempotenceFixture) {
			fx.wf.workflows["wf-1"].RuntimeVersion = wfdir.RuntimeQuery
			setManifestRuntime(t, fx.root, wfdir.RuntimeDefault)
			asCopy(t, fx)
		}, true, syncReasonNotDeployed},
		"copy: language changed": {[]string{"workflow"}, func(t *testing.T, fx idempotenceFixture) {
			fx.wf.workflows["wf-1"].Language = wfdir.LanguageJS
			fx.edit(t)
			asCopy(t, fx)
		}, true, syncReasonNotDeployed},
		"copy: app entrypoint changed": {[]string{"data app"}, func(t *testing.T, fx idempotenceFixture) {
			fx.app.apps["app-1"].Entrypoint = "Main.tsx"
			asCopy(t, fx)
		}, true, syncReasonNotDeployed},
		"copy: live equals folder": {all, asCopy, false, ""},
		"copy: recorded version current": {all, func(t *testing.T, fx idempotenceFixture) {
			fx.edit(t)
			asCopy(t, fx)
		}, false, ""},
		"checkout: local baseline and its own pushed draft": {all, func(t *testing.T, fx idempotenceFixture) {
			fx.edit(t)
			if out, err := runCLI(t, fx.root, fx.draftKind.push...); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
		}, false, ""},
		"copy: app renamed elsewhere since the recorded version": {[]string{"data app"}, func(t *testing.T, fx idempotenceFixture) {
			fx.app.apps["app-1"].Name = "Dashboard"
			setAppTitle(t, fx.root, "Dashboard")
			setAppliedTitle(t, managedFakeOf(fx), fx.root, "prod", "Dashboard")
			renameLive(fx, "Renamed in the web app")
			asCopy(t, fx)
		}, true, ""},
		"copy: workflow renamed by the folder, applied title recorded": {[]string{"workflow"}, func(t *testing.T, fx idempotenceFixture) {
			setAppliedTitle(t, managedFakeOf(fx), fx.root, "prod", "Monthly report")
			fx.draftKind.retitle(t, "Quarterly report")
			asCopy(t, fx)
		}, false, ""},
		"copy: workflow renamed by the folder, no applied title recorded": {[]string{"workflow"}, func(t *testing.T, fx idempotenceFixture) {
			setAppliedTitle(t, managedFakeOf(fx), fx.root, "prod", "")
			fx.draftKind.retitle(t, "Quarterly report")
			asCopy(t, fx)
		}, true, ""},
	}
	fixtures := draftKindFixtures()
	for name, r := range rows {
		for _, kind := range r.kinds {
			t.Run(kind+"/"+name, func(t *testing.T) {
				fx := fixtures[kind](t)
				adoptLine(t, fx.tree, "prod", filepath.Base(fx.root))
				r.stage(t, fx)
				dry := previewLine(t, fx)
				_, applied, _, _ := applyOnce(t, fx)
				if (applied.Verdict == verdictRefused) != r.refused {
					t.Fatalf("the fixture is wrong: apply answered %s (%s) %s", applied.Verdict, applied.Reason, applied.Detail)
				}
				if (dry.Verdict == verdictRefused) != r.refused {
					t.Errorf("the dry run says %s (%s) %s; apply says %s (%s) %s",
						dry.Verdict, dry.Reason, dry.Detail, applied.Verdict, applied.Reason, applied.Detail)
				}
				if r.refused && dry.Reason != applied.Reason {
					t.Errorf("the dry run refuses with %s, apply with %s", dry.Reason, applied.Reason)
				}
				if r.reason != "" && (dry.Reason != r.reason || applied.Reason != r.reason) {
					t.Errorf("the dry run refuses with %s (%s), apply with %s (%s); want %s", dry.Reason, dry.Detail, applied.Reason, applied.Detail, r.reason)
				}
			})
		}
	}
}

// A workflow never published is a parentless draft, which the preview asks the
// push's check of as the push does.
func TestDryRunAgreesWithApplyForANeverPublishedWorkflow(t *testing.T) {
	for name, forget := range map[string]bool{"version recorded at create": false, "no version recorded": true} {
		t.Run(name, func(t *testing.T) {
			f := newFakeInstance(t)
			signIn(t, f)
			tree := t.TempDir()
			root := filepath.Join(tree, "report")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
				t.Fatalf("init: %v", err)
			}
			writeLocal(t, root, "main.py", "body\n")
			if out, err := runCLI(t, root, "wf", "push"); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			if forget {
				forgetHeadVersion(t, f.managed, root, "dev")
			}
			adoptLine(t, tree, "dev", "report")
			writeLocal(t, root, "main.py", "body\nchanged\n")
			dropLocalState(t, tree)

			out, _ := runCLI(t, tree, "sync", "apply", "--stack", "dev", "--dry-run", "--json")
			dry := applyFolderLine(t, applyReportOf(t, out), "report")
			out, _ = runCLI(t, tree, "sync", "apply", "--stack", "dev", "--json")
			applied := applyFolderLine(t, applyReportOf(t, out), "report")
			if (dry.Verdict == verdictRefused) != (applied.Verdict == verdictRefused) || dry.Reason != applied.Reason {
				t.Errorf("the dry run says %s (%s) %s; apply says %s (%s) %s",
					dry.Verdict, dry.Reason, dry.Detail, applied.Verdict, applied.Reason, applied.Detail)
			}
		})
	}
}

// The no-recorded-version refusal under `sync apply` names what works from a
// copy: adopt and propose, never --force or a re-clone.
func TestApplyRefusalWithoutRecordedVersionNamesAdopt(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			adoptLine(t, fx.tree, "prod", filepath.Base(fx.root))
			forgetHeadVersion(t, managedFakeOf(fx), fx.root, "prod")
			fx.edit(t)
			dropLocalState(t, fx.root)
			_, line, _, _ := applyOnce(t, fx)
			if !strings.Contains(line.Detail, "ronja sync adopt --stack prod") || !strings.Contains(line.Detail, "ronja propose") ||
				strings.Contains(line.Detail, "--force") || strings.Contains(line.Detail, "fresh folder") {
				t.Errorf("apply's refusal: %s %s", line.Verdict, line.Detail)
			}
		})
	}
}

// proposeCase is the fresh-stack fixture after its first deploy, as a copy
// would see it: no local baseline anywhere.
func proposeCase(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f, dir, _ := freshStackFixture(t)
	if report, err := applyStack(t, dir, "dev", "--allow-create"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the first deploy: %v\n%s", err, describeApply(report))
	}
	dropLocalState(t, dir)
	return f, dir
}

// deployedCopyVerdict applies a proposal's copy and answers its verdict.
func deployedCopyVerdict(t *testing.T, archive []byte) string {
	t.Helper()
	report, _ := applyStack(t, unpackCopy(t, archive), "dev", "--allow-create")
	return report.Verdict + ": " + describeApply(report)
}

// An adopted folder whose stack records no live version, live differing from
// it: the copy's deploy would refuse, so propose refuses first, with the
// remedy, and uploads nothing.
func TestProposeRefusesWhatTheCopysDeployRefuses(t *testing.T) {
	for _, o := range ownFolders {
		t.Run(o.path, func(t *testing.T) {
			f, dir := proposeCase(t)
			root := filepath.Join(dir, o.path)
			forgetHeadVersion(t, f.managed, root, "dev")
			writeLocal(t, root, o.file, readFile(t, root, o.file)+"\n# proposed\n")

			out, err := runPropose_(t, dir, "--stack", "dev", "--json")
			if err == nil {
				t.Fatalf("propose went through, and its copy's deploy answered %s", deployedCopyVerdict(t, f.changeLists.proposals[0].archive))
			}
			if !strings.Contains(err.Error(), o.path) || !strings.Contains(err.Error(), "ronja sync adopt --stack dev") {
				t.Errorf("the refusal does not name the folder and the remedy: %v\n%s", err, out)
			}
			if n := len(f.changeLists.proposals); n != 0 {
				t.Errorf("%d proposal(s) uploaded", n)
			}
		})
	}
}

// propose lets through the proposer's own open draft as an update the
// approval shows; a colleague's module draft still refuses, naming them.
func TestProposeLetsTheProposersOwnDraftThrough(t *testing.T) {
	type draftCase struct {
		folder  string
		open    func(t *testing.T, f *fakeImportInstance, root string) string
		through bool
		names   string
	}
	cases := map[string]draftCase{
		"workflow": {folder: "workflows/sales-report", through: true, open: func(t *testing.T, f *fakeImportInstance, root string) string {
			id := mustLoadLock(t, root).Stacks["dev"].WorkflowID
			return f.wf.AddDraft(id, "draft-own", api.WorkflowFile{Path: "main.py", Content: "work in progress\n"}).ID
		}},
		"data app": {folder: "apps/sales-dashboard", through: true, open: func(t *testing.T, f *fakeImportInstance, root string) string {
			id := mustLoadLock(t, root).Stacks["dev"].DataAppID
			return f.app.AddDraft(id, "data_app-draft-own", api.DataAppFile{Path: "App.tsx", Content: "work in progress\n"}).ID
		}},
		"module, the proposer's own": {folder: "modules/sales_helpers", through: true, open: func(t *testing.T, f *fakeImportInstance, root string) string {
			id := mustLoadLock(t, root).Stacks["dev"].ModuleID
			return openModuleDraft(f.wf, id, "module-draft-own", fakeCallerUserID, fakeCallerUserID, "money.py", "work in progress\n")
		}},
		"module, a colleague's": {folder: "modules/sales_helpers", through: false, names: "usr-colleague", open: func(t *testing.T, f *fakeImportInstance, root string) string {
			id := mustLoadLock(t, root).Stacks["dev"].ModuleID
			return openModuleDraft(f.wf, id, "module-draft-theirs", "usr-colleague", "usr-colleague", "money.py", "their work\n")
		}},
		"module, drafted by the proposer, last edited by a colleague": {folder: "modules/sales_helpers", through: false, names: "usr-colleague", open: func(t *testing.T, f *fakeImportInstance, root string) string {
			id := mustLoadLock(t, root).Stacks["dev"].ModuleID
			return openModuleDraft(f.wf, id, "module-draft-shared", fakeCallerUserID, "usr-colleague", "money.py", "their work\n")
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, dir := proposeCase(t)
			root := filepath.Join(dir, c.folder)
			draftID := c.open(t, f, root)
			for _, o := range ownFolders {
				if o.path == c.folder {
					writeLocal(t, root, o.file, readFile(t, root, o.file)+"\n# proposed\n")
				}
			}

			out, err := runPropose_(t, dir, "--stack", "dev", "--json")
			if !c.through {
				if err == nil {
					t.Fatalf("propose went through over a colleague's draft:\n%s", out)
				}
				if !strings.Contains(err.Error(), c.names) || !strings.Contains(err.Error(), draftID) {
					t.Errorf("the refusal does not name the colleague and the draft: %v", err)
				}
				if n := len(f.changeLists.proposals); n != 0 {
					t.Errorf("%d proposal(s) uploaded", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("propose refused the proposer's own draft: %v\n%s", err, out)
			}
			plan := uploadedPlan(t, f.changeLists.proposals[0].plan)
			line := folderLine(t, syncApplyReport{Folders: plan.Folders}, c.folder)
			if line.Verdict != verdictApplied || len(line.Units) != 1 {
				t.Fatalf("the folder was uploaded as %s (%s) %s", line.Verdict, line.Reason, line.Detail)
			}
			u := line.Units[0]
			if u.Action != string(applyActionUpdate) || u.Draft == nil || u.Draft.ID != draftID || !strings.Contains(u.Detail, draftID) ||
				!strings.Contains(u.Detail, "your open draft "+draftID+" (last edited ") || !strings.HasSuffix(u.Detail, ") is discarded when this list deploys") {
				t.Errorf("the unit was uploaded as %s (%s) %s, draft %+v", u.Action, u.Reason, u.Detail, u.Draft)
			}
		})
	}
}

// With the proposer's own draft open, a rename the deploy would refuse once
// that draft is discarded refuses the proposal, naming the rename. A
// colleague's module draft is named only once the title is fixed.
func TestProposeRefusesARenameBehindAnOpenDraft(t *testing.T) {
	cases := map[string]struct {
		folder, kind string
		open         func(f *fakeImportInstance, liveID string) string
		colleague    bool
	}{
		"workflow": {folder: "workflows/sales-report", kind: "workflow", open: func(f *fakeImportInstance, liveID string) string {
			return f.wf.AddDraft(liveID, "draft-own", api.WorkflowFile{Path: "main.py", Content: "work in progress\n"}).ID
		}},
		"module, a colleague's": {folder: "modules/sales_helpers", kind: "module", colleague: true, open: func(f *fakeImportInstance, liveID string) string {
			return openModuleDraft(f.wf, liveID, "module-draft-theirs", "usr-colleague", "usr-colleague", "money.py", "their work\n")
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, dir := proposeCase(t)
			root := filepath.Join(dir, c.folder)
			stack := mustLoadLock(t, root).Stacks["dev"]
			liveID := cmp.Or(stack.WorkflowID, stack.ModuleID)
			draftID := c.open(f, liveID)
			for _, o := range ownFolders {
				if o.path == c.folder {
					writeLocal(t, root, o.file, readFile(t, root, o.file)+"\n# proposed\n")
				}
			}
			setAppliedTitle(t, f.managed, root, "dev", "")
			retitle := func(title string) {
				m := loadAnyManifest(t, root)
				m.Title = title
				if err := wfdir.SaveManifest(root, m); err != nil {
					t.Fatal(err)
				}
			}
			title := loadAnyManifest(t, root).Title
			retitle("Renamed in the repository")

			_, err := runPropose_(t, dir, "--stack", "dev", "--json")
			if err == nil {
				t.Fatalf("propose went through, and its copy's deploy answered %s", deployedCopyVerdict(t, f.changeLists.proposals[0].archive))
			}
			if !strings.Contains(err.Error(), "the live "+c.kind+" "+liveID+" is titled") || strings.Contains(err.Error(), "usr-colleague") {
				t.Errorf("the refusal does not name the rename alone: %v", err)
			}
			if n := len(f.changeLists.proposals); n != 0 {
				t.Errorf("%d proposal(s) uploaded", n)
			}
			if !c.colleague {
				return
			}
			retitle(title)
			if _, err := runPropose_(t, dir, "--stack", "dev", "--json"); err == nil || !strings.Contains(err.Error(), "usr-colleague") || !strings.Contains(err.Error(), draftID) {
				t.Errorf("with the title fixed, the refusal does not name the colleague's draft: %v", err)
			}
		})
	}
}

// uploadedPlan decodes a proposal's plan.
func uploadedPlan(t *testing.T, raw []byte) proposePlan {
	t.Helper()
	var plan proposePlan
	decodeJSONInto(t, string(raw), &plan)
	return plan
}

// failVersionReads makes every version-history read die before it reaches the
// fake, as a network that is not there.
func failVersionReads(t *testing.T) {
	t.Helper()
	previous := newClient
	newClient = func(baseURL, token string) *api.Client {
		client := previous(baseURL, token)
		client.HTTP.Transport = versionsUnreachable{base: client.HTTP.Transport}
		return client
	}
	t.Cleanup(func() { newClient = previous })
}

type versionsUnreachable struct{ base http.RoundTripper }

func (v versionsUnreachable) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/versions") {
		return nil, errors.New("dial tcp: connect: connection refused")
	}
	return cmp.Or[http.RoundTripper](v.base, http.DefaultTransport).RoundTrip(req)
}

// A version history that cannot be read previews as unknown, not as a
// refusal naming `sync adopt`; the apply, which falls back, refuses naming a
// new proposal.
func TestAFailedHistoryReadIsUnknownInThePreview(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			adoptLine(t, fx.tree, "prod", filepath.Base(fx.root))
			fx.edit(t)
			dropLocalState(t, fx.root)
			failVersionReads(t)

			dry := previewLine(t, fx)
			if dry.Verdict != verdictUnknown || dry.Reason != syncReasonLookupFailed || !strings.Contains(dry.Detail, "could not read the version history") {
				t.Errorf("the dry run says %s (%s) %s, want unknown for the failed read", dry.Verdict, dry.Reason, dry.Detail)
			}
			_, applied, _, _ := applyOnce(t, fx)
			if applied.Verdict != verdictRefused || !strings.Contains(applied.Detail, "could not read the version history") ||
				!strings.Contains(applied.Detail, "approve a new proposal") || strings.Contains(applied.Detail, "sync adopt") {
				t.Errorf("apply says %s (%s) %s", applied.Verdict, applied.Reason, applied.Detail)
			}
		})
	}
}

// propose refuses a folder whose version history it could not read, and
// uploads nothing.
func TestProposeRefusesAFailedHistoryRead(t *testing.T) {
	f, dir := proposeCase(t)
	o := ownFolders[0]
	root := filepath.Join(dir, o.path)
	writeLocal(t, root, o.file, readFile(t, root, o.file)+"\n# proposed\n")
	failVersionReads(t)

	out, err := runPropose_(t, dir, "--stack", "dev", "--json")
	if err == nil || !strings.Contains(err.Error(), o.path) || !strings.Contains(err.Error(), "could not read the version history") ||
		strings.Contains(err.Error(), "sync adopt") {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	if n := len(f.changeLists.proposals); n != 0 {
		t.Errorf("%d proposal(s) uploaded", n)
	}
}
