package commands

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// THE CLASSIC-LOOP NOTICE: a clone, push or publish of a row
// another repository folder deploys says so — on stderr and in --json — and is
// never refused for it. The fakes serve `managedBy` on each kind's single-row
// GET exactly as the server does (fakeManaged.managedBy).

// otherFolderID is a folder id no fixture folder carries: the repository
// folder that manages the row in these tests.
const otherFolderID = "4d8e2f10-6b3a-4c59-9e7d-1a2b3c4d5e6f"

// otherSource is that folder's source label.
const otherSource = "github.com/acme/data//sales/orders"

// managedRowOf names the one row each idempotence fixture deploys: its claim
// kind, id, and the address a claim would hold it at.
func managedRowOf(t *testing.T, kind string) (claimKind, id, address string) {
	t.Helper()
	switch kind {
	case "workflow":
		return wfdir.ManagedWorkflow, "wf-1", ""
	case "data app":
		return wfdir.ManagedDataApp, "app-1", ""
	case "module":
		return wfdir.ManagedModule, "module-1", ""
	case "pipeline":
		return wfdir.ManagedTable, "table-orders", "orders.sql"
	case "automation":
		return wfdir.ManagedAutomation, "job-1", "nightly.json"
	}
	t.Fatalf("no row for kind %q", kind)
	return "", "", ""
}

// pushArgsOf is the fixture's single-folder push and (for the kinds that have
// one) publish.
func pushArgsOf(kind string) (push, publish []string) {
	switch kind {
	case "workflow":
		return []string{"wf", "push"}, []string{"wf", "publish"}
	case "data app":
		return []string{"app", "push"}, []string{"app", "publish"}
	case "module":
		return []string{"module", "push"}, []string{"module", "publish"}
	case "pipeline":
		return []string{"pipeline", "push"}, []string{"pipeline", "publish"}
	}
	return []string{"automation", "push"}, nil
}

// pullCommandOf is the `ronja <kind> pull` the notice names.
func pullCommandOf(kind string) string {
	switch kind {
	case "workflow":
		return "`ronja wf pull`"
	case "data app":
		return "`ronja app pull`"
	case "module":
		return "`ronja module pull`"
	case "pipeline":
		return "`ronja pipeline pull`"
	}
	return "`ronja automation pull`"
}

// runCapturing is runCLI with stderr captured.
func runCapturing(t *testing.T, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr = captureStderr(t, func() { stdout, err = runCLI(t, dir, args...) })
	return stdout, stderr, err
}

const noticeWhere = `managed in git (github.com/acme/data, folder sales/orders, stack "prod")`

// TestClassicPushAndPublishOfAManagedRowPrintNotice: a classic folder (no
// folder id) pushing and publishing a row another folder manages is told who
// manages it, on stderr and under `managedInGit` in --json — and the push
// lands anyway.
func TestClassicPushAndPublishOfAManagedRowPrintNotice(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			m := managedFakeOf(fx)
			claimKind, id, address := managedRowOf(t, kind)
			m.stage(featureOf(t, fx.root), otherFolderID, address, claimKind, id, otherSource)

			fx.edit(t)
			push, publish := pushArgsOf(kind)
			out, stderr, err := runCapturing(t, fx.root, append(push, "--json")...)
			if err != nil {
				t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
			}
			if !strings.Contains(stderr, noticeWhere) || !strings.Contains(stderr, pullCommandOf(kind)) ||
				!strings.Contains(stderr, "this change will show as drift there") {
				t.Fatalf("push printed no managed-in-git notice:\n%s", stderr)
			}
			if n := strings.Count(stderr, "managed in git"); n != 1 {
				t.Errorf("the notice was printed %d times, want once:\n%s", n, stderr)
			}
			assertManagedInGitJSON(t, out, claimKind, id)

			if publish == nil {
				if !fx.deployed() {
					t.Fatalf("the push did not land — a notice must never block")
				}
				return
			}
			out, stderr, err = runCapturing(t, fx.root, append(publish, "--json")...)
			if err != nil {
				t.Fatalf("publish: %v\n%s\n%s", err, out, stderr)
			}
			if !strings.Contains(stderr, noticeWhere) {
				t.Fatalf("publish printed no managed-in-git notice:\n%s", stderr)
			}
			assertManagedInGitJSON(t, out, claimKind, id)
			if !fx.deployed() {
				t.Fatalf("the publish did not land — a notice must never block")
			}
		})
	}
}

// assertManagedInGitJSON checks a --json payload names the managing folder.
func assertManagedInGitJSON(t *testing.T, out, claimKind, id string) {
	t.Helper()
	var payload struct {
		ManagedInGit []managedNotice `json:"managedInGit"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode --json: %v\n%s", err, out)
	}
	for _, n := range payload.ManagedInGit {
		if n.ResourceID == id && n.Kind == claimKind && n.FolderID == otherFolderID &&
			n.SourceLabel == otherSource && n.StackLabel == "prod" && strings.Contains(n.Notice, "managed in git") {
			return
		}
	}
	t.Fatalf("--json carries no managedInGit entry for %s %s: %+v\n%s", claimKind, id, payload.ManagedInGit, out)
}

// TestClassicCloneOfAManagedRowPrintsNotice: clone stays classic — cloning a managed row
// prints who manages it, and never mints a folder id.
func TestClassicCloneOfAManagedRowPrintsNotice(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		if kind == "automation" {
			continue // the automation loop has no clone
		}
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			m := managedFakeOf(fx)
			claimKind, id, address := managedRowOf(t, kind)
			m.stage(featureOf(t, fx.root), otherFolderID, address, claimKind, id, otherSource)

			arg := id
			if kind == "pipeline" {
				arg = "collection-1"
			}
			cmd := map[string]string{"workflow": "wf", "data app": "app", "module": "module", "pipeline": "pipeline"}[kind]
			dest := filepath.Join(t.TempDir(), "copy")
			out, stderr, err := runCapturing(t, t.TempDir(), cmd, "clone", arg, dest, "--json")
			if err != nil {
				t.Fatalf("clone: %v\n%s\n%s", err, out, stderr)
			}
			if !strings.Contains(stderr, noticeWhere) || !strings.Contains(stderr, "a change made from this copy will show as drift there") ||
				!strings.Contains(stderr, pullCommandOf(kind)) {
				t.Fatalf("clone printed no managed-in-git notice:\n%s", stderr)
			}
			assertManagedInGitJSON(t, out, claimKind, id)
			if loadAnyManifest(t, dest).Adopted() {
				t.Fatalf("a clone minted a folder id")
			}
		})
	}
}

// TestTheManagingFolderPrintsNoNotice: the adopted folder that owns the claim
// — same folder id, same stack feature — deploys without a word about it.
func TestTheManagingFolderPrintsNoNotice(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			adoptFixture(t, fx)
			claimKind, id, address := managedRowOf(t, kind)
			if c := managedFakeOf(fx).ClaimAt(featureOf(t, fx.root), folderIDOf(t, fx.root), address); c == nil || c.ResourceID != id || c.Kind != claimKind {
				t.Fatalf("adopt left no claim of %s at %q: %+v", id, address, c)
			}
			fx.edit(t)
			push, publish := pushArgsOf(kind)
			for _, args := range [][]string{push, publish} {
				if args == nil {
					continue
				}
				out, stderr, err := runCapturing(t, fx.root, append(args, "--json")...)
				if err != nil {
					t.Fatalf("%v: %v\n%s\n%s", args, err, out, stderr)
				}
				if strings.Contains(stderr, "managed in git") || strings.Contains(out, "managedInGit") {
					t.Fatalf("%v from the managing folder printed the notice:\n%s\n%s", args, stderr, out)
				}
			}
			if !fx.deployed() {
				t.Fatalf("the managing folder's deploy did not land")
			}
		})
	}
}

// TestAnUnmanagedRowPrintsNoNotice: no claim, no notice — and an instance
// older than `managedBy` (no GET carries it, though a claim exists) is the
// same silence, never a failure.
func TestAnUnmanagedRowPrintsNoNotice(t *testing.T) {
	for _, older := range []bool{false, true} {
		for kind, setup := range idempotenceFixtures() {
			name := kind + "/unclaimed"
			if older {
				name = kind + "/older-server"
			}
			t.Run(name, func(t *testing.T) {
				fx := setup(t)
				m := managedFakeOf(fx)
				if older {
					claimKind, id, address := managedRowOf(t, kind)
					m.stage(featureOf(t, fx.root), otherFolderID, address, claimKind, id, otherSource)
					m.noManagedBy = true
				}
				fx.edit(t)
				push, publish := pushArgsOf(kind)
				for _, args := range [][]string{push, publish} {
					if args == nil {
						continue
					}
					out, stderr, err := runCapturing(t, fx.root, append(args, "--json")...)
					if err != nil {
						t.Fatalf("%v: %v\n%s\n%s", args, err, out, stderr)
					}
					if strings.Contains(stderr, "managed in git") || strings.Contains(out, "managedInGit") {
						t.Fatalf("%v printed a notice for a row with no managedBy:\n%s\n%s", args, stderr, out)
					}
				}
				if !fx.deployed() {
					t.Fatalf("the deploy did not land")
				}
			})
		}
	}
}

// TestManagedNoticeWording pins the sentence's parts against the label
// shapes a deploy can record.
func TestManagedNoticeWording(t *testing.T) {
	cases := []struct {
		label, stack, want string
	}{
		{"github.com/acme/data//pipelines/sales", "prod", "managed in git (github.com/acme/data, folder pipelines/sales, stack \"prod\") — this change will show as drift there; `ronja wf pull` in that repository absorbs it."},
		// A folder at the repository root records the remote alone.
		{"github.com/acme/data", "prod", `managed in git (github.com/acme/data, stack "prod") — `},
		// A scheme's `://` is not the separator.
		{"https://git.example.com/acme/data//sales", "", "managed in git (https://git.example.com/acme/data, folder sales) — "},
		// A deploy from outside git records no label: the folder id is the name left.
		{"", "main", "managed in git (folder id " + otherFolderID + `, stack "main") — `},
	}
	for _, c := range cases {
		mb := &api.ManagedBy{SourceLabel: c.label, StackLabel: c.stack, FolderID: otherFolderID}
		got := describeManagedBy(wfdir.WorkflowKind, mb, noticeForChange)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("label %q: got %q, want prefix %q", c.label, got, c.want)
		}
	}
}

// TestTheSameFolderInAnotherFeatureIsNotTheOwner: an address is the folder id
// AND the feature it deploys to. The same folder, deploying to a different
// feature (another organization's copy of the repository, or a stack pointed
// elsewhere), is not the folder that manages the row, and is told so.
func TestTheSameFolderInAnotherFeatureIsNotTheOwner(t *testing.T) {
	mb := &api.ManagedBy{FolderID: otherFolderID, FeatureID: "feat-elsewhere", SourceLabel: otherSource, StackLabel: "prod"}
	f := &folder{Kind: wfdir.WorkflowKind, Stack: "prod", Binding: wfdir.Binding{FeatureID: "feat-1"},
		Manifest: &wfdir.Manifest{ID: otherFolderID, Stacks: map[string]wfdir.Stack{"prod": {}}}}
	f.noteManagedBy(wfdir.ManagedWorkflow, "wf-1", mb)
	stderr := captureStderr(t, func() { printManagedNotices(f.notices) })
	if !strings.Contains(stderr, noticeWhere) {
		t.Fatalf("the same folder id in another feature printed no notice:\n%s", stderr)
	}
	mb.FeatureID = "feat-1"
	if n := noticeManagedBy(f, f.Kind, wfdir.ManagedWorkflow, "wf-1", mb, noticeForChange); n != nil {
		t.Fatalf("the managing folder itself was told: %+v", n)
	}
}
