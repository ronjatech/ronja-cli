package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// --stack, end to end.
//
// The unit tests in internal/wfdir pin the FORMAT — what round-trips, what
// migrates, what is refused. These pin the two things only a whole command can
// show: that naming a stack on a folder that already exists UPDATES the row it
// was bound to rather than creating a second one, and that a legacy folder run
// without the flag still writes exactly the file it always wrote.

// TestPushWithStackMigratesRatherThanRecreating is the migration's whole risk in
// one assertion. A `--stack` push against a folder bound the old way must find
// the workflow it was already bound to; if the naming lost the binding, the push
// would take the create path and the customer would end up with two workflows
// and no message saying so.
func TestPushWithStackMigratesRatherThanRecreating(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")

	// The adoption notice is the only place a person is told what their
	// colleagues now need, and it must name the version this folder was actually
	// STAMPED with rather than the highest one the build can read. A
	// dependency-less folder is stamped 2; naming the constant would tell its
	// author that the whole team has to be on a version-3 CLI for a change that
	// never required one, which is the upgrade nobody has time for.
	notice := captureStderr(t, func() {
		if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err != nil {
			t.Fatalf("push --stack dev: %v", err)
		}
	})
	if !strings.Contains(notice, "format version 2") {
		t.Errorf("the adoption notice does not name the version actually stamped:\n%s", notice)
	}
	for _, req := range f.Requests {
		if req == "POST /api/v2/workflow" {
			t.Fatalf("naming a stack must not lose the binding — the push CREATED a second workflow:\n%s",
				strings.Join(f.Requests, "\n"))
		}
	}

	manifest := readFile(t, wfdir.ManifestPath(root))
	if !strings.Contains(manifest, `"formatVersion": 2`) {
		t.Errorf("a stack manifest must declare version 2:\n%s", manifest)
	}
	if strings.Contains(manifest, `"instances"`) {
		t.Errorf("the migrated entry must leave instances[]:\n%s", manifest)
	}
	if strings.Contains(manifest, "wf-1") {
		t.Errorf("the workflow id is STATE and belongs in the lock file:\n%s", manifest)
	}
	lock := readFile(t, wfdir.LockPath(root))
	if !strings.Contains(lock, `"wf-1"`) || !strings.Contains(lock, `"dev"`) {
		t.Errorf("the lock must record the row this stack's deploys own:\n%s", lock)
	}

	// And the folder keeps working with no flag at all: the stack matches this
	// credential's (instance, organization), which is the ergonomics the change
	// promised not to cost anyone.
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push after migration: %v", err)
	}
}

// TestPushWithoutStackLeavesALegacyFolderAlone: the property every existing
// customer relies on. A push that changes the binding still writes the v1 shape,
// so a colleague on an older CLI can go on reading the file.
func TestPushWithoutStackLeavesALegacyFolderAlone(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")

	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}
	manifest := readFile(t, wfdir.ManifestPath(root))
	for _, unwanted := range []string{"formatVersion", "stacks"} {
		if strings.Contains(manifest, unwanted) {
			t.Errorf("a legacy push wrote %q:\n%s", unwanted, manifest)
		}
	}
	if _, err := os.Stat(wfdir.LockPath(root)); !os.IsNotExist(err) {
		t.Errorf("a legacy folder must not grow a %s (stat err = %v)", wfdir.LockName, err)
	}
}

// TestCloneWithStackWritesBothFiles: a clone is the other way a folder is born,
// and --stack has to reach it too — otherwise the only route to the new shape is
// to migrate a folder created in the old one.
func TestCloneWithStackWritesBothFiles(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "body\n"})
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("clone --stack prod: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)

	manifest := readFile(t, wfdir.ManifestPath(root))
	if !strings.Contains(manifest, `"prod"`) || !strings.Contains(manifest, `"formatVersion": 2`) {
		t.Errorf("clone --stack must write a named stack at version 2:\n%s", manifest)
	}
	if !strings.Contains(readFile(t, wfdir.LockPath(root)), `"wf-1"`) {
		t.Errorf("clone --stack must record the row in the lock file")
	}
}

// TestPushRefusesAnUnnamedNewBindingOnAStackFolder: a folder that has moved to
// stacks must not grow an unnamed legacy entry beside them. Both shapes would
// then answer for one place, and which one a later command read would depend on
// nothing legible — so the refusal asks for the one thing that cannot be
// derived, a name a person chose.
func TestPushRefusesAnUnnamedNewBindingOnAStackFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "x", Entrypoint: "main.py",
		Stacks: map[string]wfdir.Stack{
			// Somewhere else entirely, so this credential matches nothing.
			"prod": {URL: "https://app.ronja.tech", TenantID: "ten-prod", FeatureID: "feat-p"},
		},
	}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", "body\n")

	_, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "--stack") {
		t.Errorf("refusal = %v, want it to ask for a stack name", err)
	}
}

// TestPipelineCloneWithStackPutsLiveFingerprintsInTheLock: the config/state
// split applied to the one fingerprint that is genuinely per-ENVIRONMENT.
//
// "The live table held these bytes when this folder last agreed with it" is the
// same for everybody, so committing it is what finally gives a fresh CI checkout
// something to compare against. The draft fingerprints beside it describe one
// caller's own open draft and must stay in .ronja/, or a colleague inherits a
// baseline for a row they cannot see.
func TestPipelineCloneWithStackPutsLiveFingerprintsInTheLock(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")

	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out",
		"--stack", "dev", "--json"); err != nil {
		t.Fatalf("clone --stack dev: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")

	lock := readFile(t, root, wfdir.LockName)
	if !strings.Contains(lock, "liveSHA256") || !strings.Contains(lock, "table-orders") {
		t.Errorf("the live fingerprints belong in %s:\n%s", wfdir.LockName, lock)
	}
	if strings.Contains(lock, "draftSHA256") || strings.Contains(lock, "draftID") {
		t.Errorf("a draft is per-USER and must never reach a committed file:\n%s", lock)
	}
	state := readFile(t, root, wfdir.StateDirName, wfdir.StateFileName)
	if strings.Contains(state, "liveSHA256") {
		t.Errorf("with a stack, the live fingerprint must not also be written locally:\n%s", state)
	}
}

// TestBadStackIsRefusedByStatusToo: `status` degrades around an ENVIRONMENT it
// cannot reach, which is its whole point. It does not degrade around a wrong
// argument — there is no report to give about a stack that does not exist, and
// giving one under the "bound to several organizations" heading would describe a
// problem the reader does not have.
func TestBadStackIsRefusedByStatusToo(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "body\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err != nil {
		t.Fatalf("push --stack dev: %v", err)
	}

	_, err := runCLI(t, root, "wf", "status", "--stack", "dv")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "--stack dev") {
		t.Errorf("refusal = %v, want it to name the stack that was meant", err)
	}
}

// TestStackIsCheckedAgainstTheOrganizationTheTokenReaches is the CI-safety
// assertion for --stack, and the reason a declared stack does not skip the
// `GET /me` an environment token already pays.
//
// The stack says which organization the folder MEANS. The token says which one
// it REACHES. When a stale declaration or a token exported for the wrong
// organization makes those differ, the command has to say so: resolving the
// stack's featureID under a token that cannot see it produces a 404, and every
// push path reads a 404 on a bound row as "create it".
func TestStackIsCheckedAgainstTheOrganizationTheTokenReaches(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "x", Entrypoint: "main.py",
		Stacks: map[string]wfdir.Stack{
			// The fake's own URL, but an organization its /me does not report.
			"dev": {URL: f.URL(), TenantID: "ten-somebody-else", FeatureID: "feat-1"},
		},
	}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", "body\n")

	_, err := runCLI(t, root, "wf", "push", "--stack", "dev")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "ten-somebody-else") || !strings.Contains(err.Error(), testTenantID) {
		t.Errorf("refusal = %v, want it to name both organizations", err)
	}
}

// TestPipelinePushPersistsTheLiveFingerprintOnAStack is the regression for the
// bug the live-fingerprint move introduced and would have shipped silently.
//
// `pipeline push` records a live agreement on the ordinary path, not only when
// it creates a table, and for a stack folder that write lands in the LOCK. The
// lock was originally saved only alongside the manifest, i.e. only on a create —
// so the fingerprint lived in memory, died with the process, and every later
// push read "no baseline" and adopted whatever the live table held. A drift
// guard disarmed by its own writer is worse than no guard, because `status`
// keeps reporting clean.
func TestPipelinePushPersistsTheLiveFingerprintOnAStack(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")

	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "init", "--feature", "collection-1",
		"--stack", "dev", "--json"); err != nil {
		t.Fatalf("init --stack dev: %v\n%s", err, stderr)
	}
	writeLocal(t, dir, "margin.sql", "SELECT 1")
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}

	lock := readFile(t, dir, wfdir.LockName)
	if !strings.Contains(lock, "liveSHA256") {
		t.Errorf("the live fingerprint must survive the process that took it:\n%s", lock)
	}
	if !strings.Contains(lock, "margin.sql") {
		t.Errorf("the created table must be recorded in %s:\n%s", wfdir.LockName, lock)
	}
}

// TestNamingAStackCarriesThePipelineLiveFingerprintsAcross: the migration has to
// move the live fingerprints from the local baseline into the lock at the moment
// it happens.
//
// Left behind, they would be correct for whoever ran the migration and invisible
// to everybody else until a publish happened to re-record them — and the whole
// claim of the lock file is that naming a stack gives an automated job something
// committed to compare against.
func TestNamingAStackCarriesThePipelineLiveFingerprintsAcross(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")

	dir := t.TempDir()
	// A LEGACY folder first: no --stack, so the fingerprints land in .ronja/.
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out", "--json"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")
	if _, err := os.Stat(filepath.Join(root, wfdir.LockName)); !os.IsNotExist(err) {
		t.Fatalf("a legacy clone must not write a lock file (stat err = %v)", err)
	}
	local := readFile(t, root, wfdir.StateDirName, wfdir.StateFileName)
	if !strings.Contains(local, "liveSHA256") {
		t.Fatalf("the legacy clone should have recorded live fingerprints locally:\n%s", local)
	}

	// Now name it. Any acting command does it; `status` deliberately does not.
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "dev"); err != nil {
		t.Fatalf("push --stack dev: %v\n%s", err, stderr)
	}
	lock := readFile(t, root, wfdir.LockName)
	if !strings.Contains(lock, "liveSHA256") {
		t.Errorf("naming the stack must carry the live fingerprints into %s:\n%s", wfdir.LockName, lock)
	}
}

// TestMissingFeatureAdviceNamesTheStack: the "no feature recorded" refusal has
// to describe a line the reader's file actually has. On a stack folder the
// feature lives at stacks.<name>.featureID, and telling them to add "featureID"
// to "the <url> entry" sends them looking for a key that is not there — which is
// the same defect the surrounding refusals were rewritten to stop, arriving
// through the door this change opened.
func TestMissingFeatureAdviceNamesTheStack(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "x", Entrypoint: "main.py",
		// Declared, pointed here, and carrying no featureID.
		Stacks: map[string]wfdir.Stack{"dev": {URL: f.URL(), TenantID: testTenantID}},
	}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", "body\n")

	_, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), `"dev" stack`) {
		t.Errorf("refusal = %v, want it to name the stack the key belongs on", err)
	}
}

// TestInitWithStackThenPlainPushKeepsOneBinding is the B2 regression, for the
// two kinds that did not have the guard `pipeline push` already had.
//
// `wf init --stack dev` leaves a folder that DECLARES a stack and has no row —
// bound and creating at once. The create path called bindNew unconditionally,
// which overwrote the resolved stack with an empty --stack and recorded the new
// id in a legacy instances[] entry BESIDE the declared stack. The folder then
// named the same (instance, organization) twice, every later command refused as
// ambiguous, and the documented recovery — `--stack dev` — took the create path
// again and produced a SECOND workflow.
//
// Reached the same way by `wf discard --delete-workflow`, which empties the
// stack's lock entry and leaves exactly this shape.
func TestInitWithStackThenPlainPushKeepsOneBinding(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("init --stack dev: %v", err)
	}
	writeLocal(t, root, "main.py", "body\n")

	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}

	manifest := readFile(t, wfdir.ManifestPath(root))
	if strings.Contains(manifest, `"instances"`) {
		t.Fatalf("the created id went to a legacy entry beside the declared stack:\n%s", manifest)
	}
	lock := readFile(t, wfdir.LockPath(root))
	if !strings.Contains(lock, `"dev"`) || !strings.Contains(lock, `"workflowID"`) {
		t.Fatalf("the stack's lock entry must hold the created id:\n%s", lock)
	}

	// The folder must still open, and a second push must find the row rather
	// than create another one.
	f.Requests = nil
	writeLocal(t, root, "main.py", "changed\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("second push: %v", err)
	}
	for _, req := range f.Requests {
		if req == "POST /api/v2/workflow" {
			t.Fatalf("the second push created a duplicate workflow:\n%s", strings.Join(f.Requests, "\n"))
		}
	}
}

// TestAppInitWithStackThenPlainPushKeepsOneBinding is TestInitWithStackThenPlain
// PushKeepsOneBinding for a data app: the same unconditional bindNew, the same
// split folder, the same duplicate on the recovery. The three kinds have to
// agree, which is why the third is asserted rather than assumed.
func TestAppInitWithStackThenPlainPushKeepsOneBinding(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "app", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("app init --stack dev: %v", err)
	}

	if _, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("app push: %v", err)
	}

	manifest := readFile(t, wfdir.ManifestPath(root))
	if strings.Contains(manifest, `"instances"`) {
		t.Fatalf("the created id went to a legacy entry beside the declared stack:\n%s", manifest)
	}
	lock := readFile(t, wfdir.LockPath(root))
	if !strings.Contains(lock, `"dev"`) || !strings.Contains(lock, `"dataAppID"`) {
		t.Fatalf("the stack's lock entry must hold the created id:\n%s", lock)
	}

	f.Requests = nil
	writeLocal(t, root, "src/App.tsx", "export default function App() { return null }\n")
	if _, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("second app push: %v", err)
	}
	for _, req := range f.Requests {
		if req == "POST /api/v2/dataapp" {
			t.Fatalf("the second push created a duplicate data app:\n%s", strings.Join(f.Requests, "\n"))
		}
	}
}

// TestPushNeverSendsTheLockFile is the B1 regression at the command level, which
// is where it was found. A workflow folder has no SyncExt, so everything the
// structural rules allow is pushed — and ronja.lock.json is a real, committed
// file sitting in the walk.
func TestPushNeverSendsTheLockFile(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("init --stack dev: %v", err)
	}
	writeLocal(t, root, "main.py", "body\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}

	for _, req := range f.Requests {
		if strings.Contains(req, wfdir.LockName) {
			t.Fatalf("the folder's own state was pushed into the customer's workflow:\n%s",
				strings.Join(f.Requests, "\n"))
		}
	}

	// And the folder is clean afterwards: a lock file the push had sent would
	// come back as a remote file the baseline knows nothing about, and the
	// folder would report `modified ronja.lock.json` for ever.
	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, wfdir.LockName) {
		t.Fatalf("status mentions %s, which is folder machinery:\n%s", wfdir.LockName, out)
	}
}

// TestStatusDoesNotClaimAStackTheFileDoesNotDeclare is the B8 regression.
//
// `--stack dev` on a LEGACY folder resolves through the migration path, so the
// resolved name is "dev" — but `status` deliberately does not migrate, and
// ronja.json still declares no stacks. Printing `Stack: dev` there states a fact
// about the folder that is not true until an acting command makes it true.
func TestStatusDoesNotClaimAStackTheFileDoesNotDeclare(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "body\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	out, err := runCLI(t, root, "wf", "status", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("status --stack dev: %v", err)
	}
	if stack, _ := decodeJSON(t, out)["stack"].(string); stack != "" {
		t.Errorf("status reported stack %q for a folder whose %s declares none", stack, wfdir.ManifestName)
	}
	// And status still did not rewrite anything, which is the other half of the
	// promise: the command you run when you are already suspicious.
	if strings.Contains(readFile(t, wfdir.ManifestPath(root)), `"stacks"`) {
		t.Error("status migrated the folder")
	}

	// An acting command DOES declare it, and status then reports it.
	if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err != nil {
		t.Fatalf("push --stack dev: %v", err)
	}
	out, err = runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if stack, _ := decodeJSON(t, out)["stack"].(string); stack != "dev" {
		t.Errorf("status reported stack %q after the folder declared one", stack)
	}
}

// TestPushAdoptsAndHealsAStrandedLockEntry is B5 end to end. SaveFolder writes
// the lock FIRST, so a push that created a workflow and then failed to write the
// manifest leaves the id under a name no stack declares.
//
// Two things have to happen, and the second is what makes the first worth
// having: `--stack dev` ADOPTS the id rather than creating a second workflow,
// and the push then DECLARES the stack — because until it does, a push with no
// --stack sees a manifest naming nothing here and creates the duplicate all over
// again.
func TestPushAdoptsAndHealsAStrandedLockEntry(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "body\n"})
	signIn(t, f)

	root := t.TempDir()
	if err := wfdir.SaveManifest(root, &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "x", Entrypoint: "main.py",
		Stacks: map[string]wfdir.Stack{},
	}); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, wfdir.LockName, `{"stacks":{"dev":{"workflowID":"wf-1"}}}`+"\n")
	writeLocal(t, root, "main.py", "changed\n")

	if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err != nil {
		t.Fatalf("push --stack dev: %v", err)
	}
	for _, req := range f.Requests {
		if req == "POST /api/v2/workflow" {
			t.Fatalf("a stranded lock entry must be adopted, not duplicated:\n%s", strings.Join(f.Requests, "\n"))
		}
	}
	if !strings.Contains(readFile(t, wfdir.ManifestPath(root)), `"dev"`) {
		t.Fatalf("the push must declare the stack the lock already records:\n%s",
			readFile(t, wfdir.ManifestPath(root)))
	}

	// Healed, so the folder works with no flag — which is the half that stops
	// the duplicate coming back.
	f.Requests = nil
	writeLocal(t, root, "main.py", "changed again\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push after healing: %v", err)
	}
	for _, req := range f.Requests {
		if req == "POST /api/v2/workflow" {
			t.Fatalf("the healed folder still created a duplicate:\n%s", strings.Join(f.Requests, "\n"))
		}
	}
}
