package commands

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

func setManifestPipPackages(t *testing.T, root string, packages []string) {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if packages == nil {
		m.PipPackages = nil
	} else {
		m.SetPipPackages(packages)
	}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

func pipPatchesTo(f *fakeInstance) []*[]string {
	var out []*[]string
	for _, p := range f.workflowPatches {
		if p.PipPackages != nil {
			out = append(out, p.PipPackages)
		}
	}
	return out
}

func TestPushSendsDeclaredPipPackagesOnCreateAndValidate(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestPipPackages(t, root, []string{" google-auth", "requests", "google-auth"})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	want := []string{"google-auth", "requests"}
	if len(f.created) != 1 || !reflect.DeepEqual(f.created[0].PipPackages, want) {
		t.Fatalf("created = %+v, want pipPackages %v", f.created, want)
	}
	if len(f.validated) != 1 || !reflect.DeepEqual(f.validated[0].PipPackages, want) {
		t.Errorf("validated = %+v, want pipPackages %v", f.validated, want)
	}
	if got := pipPatchesTo(f); len(got) != 0 {
		t.Errorf("a create that carried the list also patched it: %v", got)
	}
}

func TestPushPatchesAChangedPipPackagesListOnly(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, PipPackages: []string{"google-auth"}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	setManifestPipPackages(t, root, []string{"  google-auth ", ""})
	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := pipPatchesTo(f); len(got) != 0 {
		t.Fatalf("a list equal after cleaning was patched: %v", *got[0])
	}

	setManifestPipPackages(t, root, []string{"google-auth", "facebook-business==19.0.0"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("second push: %v", err)
	}
	got := pipPatchesTo(f)
	if len(got) != 1 || !reflect.DeepEqual(*got[0], []string{"google-auth", "facebook-business==19.0.0"}) {
		t.Fatalf("pip patches = %v, want the changed list once", got)
	}
}

func TestPushNeverSendsPipPackagesForAFolderThatDoesNotManageThem(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, PipPackages: []string{"google-auth"}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestPipPackages(t, root, nil)

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := pipPatchesTo(f); len(got) != 0 {
		t.Errorf("an unmanaged folder patched pipPackages: %v", *got[0])
	}
	for _, v := range f.validated {
		if v.PipPackages != nil {
			t.Errorf("an unmanaged folder validated with pipPackages %v", v.PipPackages)
		}
	}

	fresh := newFakeInstance(t)
	signIn(t, fresh)
	unmanaged := initFolder(t, fresh, map[string]string{"main.py": "x\n"})
	if _, err := runCLI(t, unmanaged, "wf", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if len(fresh.created) != 1 || fresh.created[0].PipPackages != nil {
		t.Errorf("an unmanaged create sent pipPackages: %+v", fresh.created)
	}
}

func TestPushClearsPipPackagesWhenTheManifestDeclaresNone(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, PipPackages: []string{"google-auth"}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestPipPackages(t, root, []string{})

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	got := pipPatchesTo(f)
	if len(got) != 1 || *got[0] == nil || len(*got[0]) != 0 {
		t.Fatalf("pip patches = %v, want one explicit []", got)
	}
}

func TestCloneAndInitWritePipPackages(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, PipPackages: []string{"google-auth"}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.AddWorkflow(&api.Workflow{ID: "wf-2", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)

	root := cloneFolder(t, f, "wf-1")
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	if !m.ManagesPipPackages() || !reflect.DeepEqual(m.DeclaredPipPackages(), []string{"google-auth"}) {
		t.Errorf("clone wrote pipPackages %v, want the row's", m.PipPackages)
	}
	none := cloneFolder(t, f, "wf-2")
	if m, err := wfdir.LoadManifest(none, wfdir.WorkflowKind); err != nil || m.PipPackages == nil || len(*m.PipPackages) != 0 {
		t.Errorf("clone of a workflow with none wrote %v, want []", m.PipPackages)
	}

	py := t.TempDir()
	if _, err := runCLI(t, py, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if m, err := wfdir.LoadManifest(py, wfdir.WorkflowKind); err != nil || m.PipPackages == nil || len(*m.PipPackages) != 0 {
		t.Errorf("python init wrote pipPackages %v, want []", m.PipPackages)
	}
	stageJSDefaults(f)
	js := t.TempDir()
	if _, err := runCLI(t, js, "wf", "init", "--feature", "feat-1", "--language", "js"); err != nil {
		t.Fatalf("init js: %v", err)
	}
	if m, err := wfdir.LoadManifest(js, wfdir.WorkflowJSKind); err != nil || m.PipPackages != nil {
		t.Errorf("js init wrote pipPackages %v, want no key", m.PipPackages)
	}
}

func TestPipPackagesChangedRemotelyIsDrift(t *testing.T) {
	recorded := []string{"google-auth"}
	baseline := &wfdir.InstanceState{PipPackages: &recorded}
	target := &api.Workflow{PipPackages: []string{"google-auth", "pandas"}}
	declared := []string{"requests"}
	patch := api.WorkflowPatch{PipPackages: &declared}

	drift := metadataDriftOf(baseline, target, patch)
	if len(drift) != 1 || drift[0].Field != "Python packages" {
		t.Fatalf("drift = %+v, want one Python packages line", drift)
	}
	if drift[0].Baseline != "google-auth" || drift[0].Remote != "google-auth, pandas" {
		t.Errorf("drift = %+v", drift[0])
	}

	sameAfterCleaning := &api.Workflow{PipPackages: []string{"google-auth"}}
	if got := metadataDriftOf(baseline, sameAfterCleaning, patch); len(got) != 0 {
		t.Errorf("an unchanged remote list reported drift: %+v", got)
	}
	if got := metadataDriftOf(&wfdir.InstanceState{}, target, patch); len(got) != 0 {
		t.Errorf("an unrecorded baseline reported drift: %+v", got)
	}
}

func TestPushRecordsThePushedPipPackagesInTheBaseline(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestPipPackages(t, root, []string{"google-auth"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	inst := state.For(wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID})
	if inst == nil || inst.PipPackages == nil || !reflect.DeepEqual(*inst.PipPackages, []string{"google-auth"}) {
		t.Fatalf("baseline pipPackages = %+v, want the pushed list", inst)
	}
}

func TestPullAbsorbsPipPackages(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	pullCycle(t, fx, pullArgsOf("workflow"), nil, func(t *testing.T) {
		if m := loadAnyManifest(t, fx.root); !m.ManagesPipPackages() {
			t.Fatalf("the cloned fixture does not manage pipPackages")
		}
		fx.wf.mu.Lock()
		fx.wf.workflows["wf-1"].Title = "Renamed in the web app"
		fx.wf.workflows["wf-1"].PipPackages = []string{"google-auth"}
		fx.wf.mu.Unlock()
	}, false)
	if got := loadAnyManifest(t, fx.root).DeclaredPipPackages(); !reflect.DeepEqual(got, []string{"google-auth"}) {
		t.Fatalf("pipPackages after pull = %v, want the live list", got)
	}
}

func underStampedRefusal(version int) string {
	return `ronja.json declares pipPackages but its formatVersion is ` + strconv.Itoa(version) +
		`: set "formatVersion": 8, so an older ronja refuses this folder instead of deploying it without its packages`
}

func TestAnUnderStampedPipPackagesListIsRefusedBeforeAnythingIsSent(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestPipPackages(t, root, []string{"requests"})
	declareFormatVersion(t, root, 1)

	if _, err := runCLI(t, root, "wf", "push", "--json"); err == nil || !strings.Contains(err.Error(), underStampedRefusal(1)) {
		t.Errorf("wf push: %v, want %q", err, underStampedRefusal(1))
	}
	if len(f.created) != 0 || len(f.validated) != 0 {
		t.Fatalf("push sent an under-stamped list: created %d, validated %d", len(f.created), len(f.validated))
	}
	out, err := runCLI(t, root, "wf", "validate", "--json")
	if err == nil {
		t.Fatal("wf validate passed an under-stamped list")
	}
	if refusals := toStrings(decodeJSON(t, out)["aliasRefusals"]); len(refusals) != 1 || refusals[0] != underStampedRefusal(1) {
		t.Errorf("validate aliasRefusals = %q, want %q", refusals, underStampedRefusal(1))
	}
	if len(f.created) != 0 {
		t.Fatalf("validate created %d workflows", len(f.created))
	}

	setManifestPipPackages(t, root, []string{"requests"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push after formatVersion 8 was set: %v", err)
	}
	if len(f.created) != 1 || !reflect.DeepEqual(f.created[0].PipPackages, []string{"requests"}) {
		t.Errorf("created = %+v, want the list", f.created)
	}
}

func TestAnEmptyPipPackagesListAtVersionOneIsNotRefused(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestPipPackages(t, root, []string{})
	if raw := readFile(t, root, wfdir.ManifestName); strings.Contains(raw, "formatVersion") {
		t.Fatalf("an empty list was stamped:\n%s", raw)
	}
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push of [] at version 1: %v", err)
	}
}

func TestSyncRefusesAnUnderStampedPipPackagesList(t *testing.T) {
	f, dir := orderTree(t)
	root := adoptedFolder(t, f, dir, "workflows/a",
		&wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow a", Entrypoint: "main.py", PipPackages: &[]string{"requests"}},
		nil, map[string]string{"main.py": "x\n"})
	declareFormatVersion(t, root, 6)
	encoded, _ := json.Marshal(underStampedRefusal(6))
	want := strings.Trim(string(encoded), `"`)

	check, _ := runCLI(t, dir, "sync", "check", "--stack", "dev", "--json")
	if !strings.Contains(check, want) {
		t.Errorf("sync check did not carry the refusal:\n%s", check)
	}
	report, _ := applyStack(t, dir, "dev", "--allow-create")
	if line := applyFolderLine(t, &report, "workflows/a"); line.Verdict != verdictRefused || !strings.Contains(line.Detail, underStampedRefusal(6)) {
		t.Errorf("sync apply line = %+v, want a refusal carrying %q", line, underStampedRefusal(6))
	}
	if flows := workflowsIn(f, devFeature); len(flows) != 0 {
		t.Errorf("sync apply created %d workflows from an under-stamped folder", len(flows))
	}
}

func TestWorkflowStatusReportsAChangedPipPackagesList(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, PipPackages: []string{"google-auth"}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	out, err := runCLI(t, root, "wf", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "Python packages: google-auth\n") || !strings.Contains(out, "    none\n") {
		t.Errorf("a clean folder's status:\n%s", out)
	}

	setManifestPipPackages(t, root, []string{"google-auth", "requests"})
	out, err = runCLI(t, root, "wf", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "changed   Python packages: google-auth, requests here, google-auth there") || strings.Contains(out, "    none\n") {
		t.Errorf("status does not report the changed list:\n%s", out)
	}

	out, err = runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	report := decodeJSON(t, out)
	if report["managesPipPackages"] != true || !reflect.DeepEqual(report["pipPackages"], []any{"google-auth", "requests"}) {
		t.Errorf("managesPipPackages = %v, pipPackages = %#v", report["managesPipPackages"], report["pipPackages"])
	}
	remote, _ := report["remote"].(map[string]any)
	if got, _ := remote["pipPackages"].(map[string]any); !reflect.DeepEqual(got["local"], []any{"google-auth", "requests"}) ||
		!reflect.DeepEqual(got["remote"], []any{"google-auth"}) {
		t.Errorf("remote.pipPackages = %#v", remote["pipPackages"])
	}

	setManifestPipPackages(t, root, []string{})
	out, err = runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	report = decodeJSON(t, out)
	if report["managesPipPackages"] != true || !reflect.DeepEqual(report["pipPackages"], []any{}) {
		t.Errorf("a folder that declares none: managesPipPackages = %v, pipPackages = %#v", report["managesPipPackages"], report["pipPackages"])
	}
	remote, _ = report["remote"].(map[string]any)
	if got, _ := remote["pipPackages"].(map[string]any); !reflect.DeepEqual(got["local"], []any{}) {
		t.Errorf("remote.pipPackages = %#v, want an empty local list", remote["pipPackages"])
	}

	setManifestPipPackages(t, root, nil)
	out, err = runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	report = decodeJSON(t, out)
	if _, present := report["pipPackages"]; present || report["managesPipPackages"] != false {
		t.Errorf("an unmanaged folder: managesPipPackages = %v, pipPackages = %#v", report["managesPipPackages"], report["pipPackages"])
	}
}

func TestWorkflowStatusNotesAnUnderStampedPipPackagesList(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestPipPackages(t, root, []string{"requests"})
	declareFormatVersion(t, root, 1)

	var err error
	stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "status") })
	if err != nil {
		t.Fatalf("status refused an under-stamped folder: %v", err)
	}
	if !strings.Contains(stderr, underStampedRefusal(1)) {
		t.Errorf("status did not say what a push refuses:\n%s", stderr)
	}
}

func TestAnInvalidPipPackageStopsThePushBeforeAnythingIsWritten(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestPipPackages(t, root, []string{"-r x"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err == nil {
		t.Fatal("a push with an invalid specifier went through")
	}
	if len(f.created) != 0 {
		t.Errorf("an invalid specifier created %d workflows", len(f.created))
	}

	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	cloned := cloneFolder(t, f, "wf-1")
	setManifestPipPackages(t, cloned, []string{"-r x"})
	_, err := runCLI(t, cloned, "wf", "push", "--json")
	if err == nil {
		t.Fatal("a push with an invalid specifier went through")
	}
	if !strings.Contains(err.Error(), "nothing was saved") {
		t.Errorf("refusal = %v, want the validate gate's", err)
	}
	if got := pipPatchesTo(f); len(got) != 0 {
		t.Errorf("an invalid specifier was patched: %v", *got[0])
	}
}
