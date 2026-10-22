package commands

import (
	"slices"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A maintainer of a shared feature commits there as an admin does: each
// publish routes on the draft's canDecide (or, for a module, on the commit's
// own answer), never on the caller's role alone.

func TestPublishSharedParentAsAMaintainerCommits(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 50
	f.maintains = true
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "organization"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	out, err := runCLI(t, root, "wf", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if decodeJSON(t, out)["outcome"] != outcomePublished || len(f.committed) != 1 {
		t.Errorf("a maintainer's publish did not commit: %s (committed %v)", out, f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("a maintainer was sent to review: %v", f.reviewRequested)
	}
}

func TestAppPublishSharedAsAMaintainerCommits(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	draft := f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "organization"
	f.privilegeLevel = 50
	f.maintains = true
	signInApp(t, f)
	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})

	out, err := runCLI(t, root, "app", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	var result appPublishResult
	decodeJSONInto(t, out, &result)
	if result.Outcome != outcomePublished || !slices.Contains(f.committed, draft.ID) {
		t.Errorf("a maintainer's publish did not commit: %s (committed %v)", out, f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("a maintainer was sent to review: %v", f.reviewRequested)
	}
}

func TestPipelinePublishCommitsAsAMaintainerInASharedFeature(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.privilegeLevel = 50
	f.maintains = true
	root := seedStagedFolder(t, f, scopeOrganization)

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if len(f.committed) != 1 || len(f.reviewRequested) != 0 {
		t.Errorf("a maintainer's publish: committed %v, review requests %v; want one commit and none", f.committed, f.reviewRequested)
	}
}

// moduleSharedEdit publishes a module, makes its feature shared, and stages an
// edit draft of it, as the signed-in caller.
func moduleSharedEdit(t *testing.T, f *fakeInstance) (root string, live *api.Module) {
	t.Helper()
	root = t.TempDir()
	if _, err := runCLI(t, root, "module", "init", "emab_tracker",
		"--feature", "collection-1", "--title", "EMAB tracker"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	if err := wfdir.WriteFile(root, "tracker.py", "x = 1\n"); err != nil {
		t.Fatalf("write tracker.py: %v", err)
	}
	if _, err := runCLI(t, root, "module", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if _, err := runCLI(t, root, "module", "publish", "--json"); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	live = f.mod.rows["module-new-1"]
	if live == nil || live.Lifecycle != api.LifecycleLive {
		t.Fatalf("the first publish must leave the module live, got %+v", live)
	}
	live.FeatureScope = scopeOrganization
	if err := wfdir.WriteFile(root, "tracker.py", "x = 2\n"); err != nil {
		t.Fatalf("edit tracker.py: %v", err)
	}
	if _, err := runCLI(t, root, "module", "push", "--json"); err != nil {
		t.Fatalf("edit push: %v", err)
	}
	return root, live
}

func TestModulePublishCommitsAsAMaintainerInASharedFeature(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleUser(t, f)
	f.maintains = true
	root, live := moduleSharedEdit(t, f)

	out, err := runCLI(t, root, "module", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v (%s)", err, out)
	}
	if decodeJSON(t, out)["outcome"] != outcomePublished || f.ModuleFileContents(live.ID)["tracker.py"] != "x = 2\n" {
		t.Errorf("a maintainer's module publish did not commit: %s", out)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("a maintainer was sent to review: %v", f.reviewRequested)
	}
}

func TestModulePublishSendsANonMaintainerToReviewOnTheCommitsRefusal(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleUser(t, f)
	root, live := moduleSharedEdit(t, f)

	out, err := runCLI(t, root, "module", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v (%s)", err, out)
	}
	if decodeJSON(t, out)["outcome"] != outcomeSubmittedForReview || len(f.reviewRequested) != 1 {
		t.Errorf("a member's shared module edit: %s, review requests %v; want one review request", out, f.reviewRequested)
	}
	if f.ModuleFileContents(live.ID)["tracker.py"] != "x = 1\n" {
		t.Errorf("a member's refused commit changed the live module")
	}
}

// Against a server that predates canDecide, publish routes on the caller's
// role as such a server decides: an admin commits, anyone else goes to review.
func TestPublishWithoutCanDecideFallsBackToTheRole(t *testing.T) {
	for _, c := range []struct {
		name      string
		privilege int
		commits   bool
	}{
		{"an admin", 10, true},
		{"a member", 50, false},
	} {
		t.Run("wf "+c.name, func(t *testing.T) {
			f := newFakeInstance(t)
			f.privilegeLevel = c.privilege
			f.omitCanDecide = true
			f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "organization"},
				api.WorkflowFile{Path: "main.py", Content: "old\n"})
			signIn(t, f)
			root := cloneFolder(t, f, "wf-1")
			writeLocal(t, root, "main.py", "new\n")
			if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
				t.Fatalf("push: %v", err)
			}
			if _, err := runCLI(t, root, "wf", "publish", "--json"); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if got := len(f.committed) == 1 && len(f.reviewRequested) == 0; got != c.commits {
				t.Errorf("committed %v, review requests %v; want commit=%v", f.committed, f.reviewRequested, c.commits)
			}
		})
		t.Run("app "+c.name, func(t *testing.T) {
			f := newFakeAppInstance(t)
			live := f.AddApp(&api.DataApp{ID: "data_app-1"})
			f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
			f.featureScope["feat-1"] = "organization"
			f.privilegeLevel = c.privilege
			f.omitCanDecide = true
			signInApp(t, f)
			root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})
			if _, err := runCLI(t, root, "app", "publish", "--json"); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if got := len(f.committed) == 1 && len(f.reviewRequested) == 0; got != c.commits {
				t.Errorf("committed %v, review requests %v; want commit=%v", f.committed, f.reviewRequested, c.commits)
			}
		})
		t.Run("pipeline "+c.name, func(t *testing.T) {
			f := newFakePipelineInstance(t)
			signInPipeline(t, f)
			f.privilegeLevel = c.privilege
			f.omitCanDecide = true
			root := seedStagedFolder(t, f, scopeOrganization)
			if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish"); err != nil {
				t.Fatalf("publish: %v\n%s", err, stderr)
			}
			if got := len(f.committed) == 1 && len(f.reviewRequested) == 0; got != c.commits {
				t.Errorf("committed %v, review requests %v; want commit=%v", f.committed, f.reviewRequested, c.commits)
			}
		})
	}
}
