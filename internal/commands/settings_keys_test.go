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

// The settings keys (description, useDedicatedCompute, serverDataOnly) follow
// the manifest's three-state rule: absent leaves the row's value alone, a
// value makes the row match.

func cloneApp(t *testing.T, f *fakeAppInstance, id string) string {
	t.Helper()
	out, err := runCLI(t, t.TempDir(), "app", "clone", id, "--json")
	if err != nil {
		t.Fatalf("app clone: %v\n%s", err, out)
	}
	return decodeJSON(t, out)["root"].(string)
}

func editFolderManifest(t *testing.T, root string, kind wfdir.Kind, edit func(m *wfdir.Manifest)) {
	t.Helper()
	m, err := wfdir.LoadManifest(root, kind)
	if err != nil {
		t.Fatal(err)
	}
	edit(m)
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

func TestCloneWritesTheSettings(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Description: "Sends the report.", UseDedicatedCompute: true},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.AddWorkflow(&api.Workflow{ID: "wf-2", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)

	root := cloneFolder(t, f, "wf-1")
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	if m.Description == nil || *m.Description != "Sends the report." || m.UseDedicatedCompute == nil || !*m.UseDedicatedCompute {
		t.Errorf("clone wrote description %v, useDedicatedCompute %v, want the row's", m.Description, m.UseDedicatedCompute)
	}
	if m.FormatVersion < 10 {
		t.Errorf("a clone with dedicated compute on is formatVersion %d, want 10", m.FormatVersion)
	}
	off := cloneFolder(t, f, "wf-2")
	raw, err := os.ReadFile(wfdir.ManifestPath(off))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"useDedicatedCompute": false`) || !strings.Contains(string(raw), `"description": ""`) {
		t.Errorf("a clone of a workflow with the settings off does not manage them:\n%s", raw)
	}
	if strings.Contains(string(raw), `"formatVersion": 10`) {
		t.Errorf("a setting that is off stamped version 10:\n%s", raw)
	}

	apps := newFakeAppInstance(t)
	apps.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive, Description: "Tracks RFQs.", ServerDataOnly: true},
		api.DataAppFile{Path: "App.tsx", Content: "x\n"})
	signInApp(t, apps)
	am := appManifestOf(t, cloneApp(t, apps, "app-1"))
	if am.Description == nil || *am.Description != "Tracks RFQs." || am.ServerDataOnly == nil || !*am.ServerDataOnly {
		t.Errorf("app clone wrote description %v, serverDataOnly %v, want the row's", am.Description, am.ServerDataOnly)
	}
	if am.UseDedicatedCompute != nil {
		t.Errorf("app clone wrote useDedicatedCompute %v", *am.UseDedicatedCompute)
	}
}

func TestCreateCarriesTheSettings(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	editFolderManifest(t, root, wfdir.WorkflowKind, func(m *wfdir.Manifest) {
		m.Description = ptr("Sends the report.")
		m.SetDedicatedCompute(true)
	})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 || f.created[0].Description != "Sends the report." || !f.created[0].UseDedicatedCompute {
		t.Fatalf("created = %+v, want the settings on the create", f.created)
	}
	for _, p := range f.workflowPatches {
		if p.Description != nil || p.UseDedicatedCompute != nil {
			t.Errorf("a create that carried the settings also patched them: %+v", p)
		}
	}

	apps := newFakeAppInstance(t)
	signInApp(t, apps)
	app := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "RFQs", Description: ptr("Tracks RFQs."), ServerDataOnly: ptr(true), FormatVersion: 10},
		map[string]string{"App.tsx": "x\n"})
	am := appManifestOf(t, app)
	am.SetBinding(apps.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(app, am); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, app, "app", "push", "--json"); err != nil {
		t.Fatalf("app push: %v\n%s", err, out)
	}
	if len(apps.created) != 1 || apps.created[0].Description != "Tracks RFQs." || !apps.created[0].ServerDataOnly {
		t.Fatalf("created = %+v, want the settings on the create", apps.created)
	}
}

func TestPushSendsAChangedSettingAndLeavesAnAbsentOneAlone(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Description: "Old.", UseDedicatedCompute: true},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	editFolderManifest(t, root, wfdir.WorkflowKind, func(m *wfdir.Manifest) { m.Description, m.UseDedicatedCompute = nil, nil })
	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	for _, p := range f.workflowPatches {
		if p.Description != nil || p.UseDedicatedCompute != nil {
			t.Fatalf("a folder managing no settings patched them: %+v", p)
		}
	}

	editFolderManifest(t, root, wfdir.WorkflowKind, func(m *wfdir.Manifest) { m.SetDedicatedCompute(false) })
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	var sent []bool
	for _, p := range f.workflowPatches {
		if p.Description != nil {
			t.Errorf("an unmanaged description was patched to %q", *p.Description)
		}
		if p.UseDedicatedCompute != nil {
			sent = append(sent, *p.UseDedicatedCompute)
		}
	}
	if len(sent) != 1 || sent[0] {
		t.Fatalf("useDedicatedCompute patches = %v, want one false", sent)
	}
	if !strings.Contains(out, "dedicated compute to off") {
		t.Errorf("the push report does not name the setting it changed:\n%s", out)
	}

	apps := newFakeAppInstance(t)
	apps.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive, Description: "Old."},
		api.DataAppFile{Path: "App.tsx", Content: "x\n"})
	signInApp(t, apps)
	app := cloneApp(t, apps, "app-1")
	editFolderManifest(t, app, wfdir.DataAppKind, func(m *wfdir.Manifest) {
		m.Description = nil
		m.SetServerDataOnly(true)
	})
	out, err = runCLI(t, app, "app", "push")
	if err != nil {
		t.Fatalf("app push: %v\n%s", err, out)
	}
	var settings []api.DataAppPatch
	for _, p := range apps.patches {
		if p.Description != nil || p.ServerDataOnly != nil {
			settings = append(settings, p)
		}
	}
	if len(settings) != 1 || settings[0].Description != nil || settings[0].ServerDataOnly == nil || !*settings[0].ServerDataOnly {
		t.Fatalf("settings patches = %+v, want one turning server-side data only on", settings)
	}
	if !strings.Contains(out, "setting  server-side data only: off → on") {
		t.Errorf("the push report does not name the setting:\n%s", out)
	}
	if apps.apps["app-1"].Description != "Old." {
		t.Errorf("an unmanaged description changed to %q", apps.apps["app-1"].Description)
	}
}

// A manifest written before the keys existed loads and saves byte for byte.
func TestAManifestWithoutTheSettingsStaysByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		kind wfdir.Kind
		body string
	}{
		{wfdir.WorkflowKind, "{\n  \"kind\": \"workflow\",\n  \"title\": \"Report\",\n  \"entrypoint\": \"main.py\",\n  \"instances\": []\n}\n"},
		{wfdir.DataAppKind, "{\n  \"kind\": \"dataapp\",\n  \"title\": \"RFQs\",\n  \"entrypoint\": \"App.tsx\",\n  \"instances\": []\n}\n"},
	} {
		kind, body := tc.kind, tc.body
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := wfdir.LoadManifest(root, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
		if raw, _ := os.ReadFile(wfdir.ManifestPath(root)); string(raw) != body {
			t.Errorf("%s manifest changed on a save:\n%s\nwant:\n%s", kind.Name, raw, body)
		}
	}
}

func TestASettingSwitchedOnNeedsVersionTen(t *testing.T) {
	on := &wfdir.Manifest{Kind: wfdir.KindWorkflow, UseDedicatedCompute: ptr(true)}
	if err := on.CheckDeclarations(); err == nil || !strings.Contains(err.Error(), `set "formatVersion": 10`) {
		t.Errorf("useDedicatedCompute true at version 1: %v, want the version-10 refusal", err)
	}
	if got := on.StampedFormatVersion(); got != 10 {
		t.Errorf("a save stamps %d, want 10", got)
	}
	app := &wfdir.Manifest{Kind: wfdir.KindDataApp, ServerDataOnly: ptr(true)}
	if err := app.CheckDeclarations(); err == nil {
		t.Error("serverDataOnly true at version 1 was accepted")
	}
	off := &wfdir.Manifest{Kind: wfdir.KindWorkflow, UseDedicatedCompute: ptr(false), Description: ptr("x")}
	if err := off.CheckDeclarations(); err != nil {
		t.Errorf("settings that switch nothing on were refused: %v", err)
	}
	if got := off.StampedFormatVersion(); got != 1 {
		t.Errorf("settings that switch nothing on stamp %d, want 1", got)
	}
}

func TestLoadRefusesASettingTheKindLacks(t *testing.T) {
	for name, body := range map[string]string{
		"dedicated compute on an app": `{"kind":"dataapp","title":"x","useDedicatedCompute":false,"instances":[]}`,
		"server data only on a flow":  `{"kind":"workflow","title":"x","serverDataOnly":false,"instances":[]}`,
		"dedicated compute on js":     `{"kind":"workflow","language":"js","formatVersion":10,"title":"x","useDedicatedCompute":true,"instances":[]}`,
	} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wfdir.LoadManifest(root, wfdir.WorkflowKind); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestSettingsChangedRemotelyAreDrift(t *testing.T) {
	baseline := &wfdir.InstanceState{Description: ptr("Old."), UseDedicatedCompute: ptr(false), ServerDataOnly: ptr(false)}
	row := &api.Workflow{Description: "Edited in the web app.", UseDedicatedCompute: true}
	patch := api.WorkflowPatch{Description: ptr("Mine."), UseDedicatedCompute: ptr(false)}
	drift := metadataDriftOf(baseline, row, patch)
	if len(drift) != 2 || drift[0].Field != "description" || drift[1].Field != "dedicated compute" || drift[1].Remote != "on" {
		t.Fatalf("workflow drift = %+v, want description and dedicated compute", drift)
	}
	if got := metadataDriftOf(baseline, row, api.WorkflowPatch{}); len(got) != 0 {
		t.Errorf("a setting the push leaves alone reported drift: %+v", got)
	}

	app := &api.DataApp{Description: "Old.", ServerDataOnly: true}
	appDrift := appSettingsDriftOf(baseline, app, api.DataAppPatch{ServerDataOnly: ptr(false), Description: ptr("Mine.")})
	if len(appDrift) != 1 || appDrift[0].Field != "server-side data only" {
		t.Fatalf("app drift = %+v, want server-side data only alone", appDrift)
	}
	if got := appSettingsDriftOf(&wfdir.InstanceState{}, app, api.DataAppPatch{ServerDataOnly: ptr(false)}); len(got) != 0 {
		t.Errorf("an unrecorded baseline reported drift: %+v", got)
	}
}

// A settings-only change plans an update whose unit names the setting, so
// the person's card shows it, never "nothing to push".
func TestADryRunOfASettingsOnlyChangeNamesTheSetting(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(t *testing.T) idempotenceFixture
		kind   wfdir.Kind
		change func(m *wfdir.Manifest)
		want   string
	}{
		"workflow": {workflowIdempotenceFixture, wfdir.WorkflowKind, func(m *wfdir.Manifest) {
			m.Description = ptr("Sends the report.")
			m.SetDedicatedCompute(true)
		}, "settings: description; dedicated compute: off → on"},
		"data app": {appIdempotenceFixture, wfdir.DataAppKind, func(m *wfdir.Manifest) { m.SetServerDataOnly(true) },
			"settings: server-side data only: off → on"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := tc.setup(t)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			editFolderManifest(t, fx.root, tc.kind, tc.change)
			_, line, _ := dryRunLine(t, fx)
			if len(line.Units) != 1 || line.Units[0].Action != string(applyActionUpdate) || line.Units[0].Detail != tc.want {
				t.Fatalf("dry run of a settings-only change: %+v, want an update detailed %q", line, tc.want)
			}
			if !strings.Contains(line.Detail, "would push and publish") {
				t.Errorf("the folder's line says %q", line.Detail)
			}
			// A push before proposing leaves a draft open; the setting is
			// still measured against live.
			if out, err := runCLI(t, fx.root, fx.draftKind.push...); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			if _, line, _ = dryRunLine(t, fx); len(line.Units) != 1 || line.Units[0].Detail != tc.want {
				t.Fatalf("dry run with the draft open: %+v, want an update detailed %q", line, tc.want)
			}
		})
	}
}

func TestPullAbsorbsTheSettings(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	pullCycle(t, fx, pullArgsOf("workflow"), nil, func(t *testing.T) {
		fx.wf.mu.Lock()
		fx.wf.workflows["wf-1"].Description = "Edited in the web app."
		fx.wf.workflows["wf-1"].UseDedicatedCompute = true
		fx.wf.workflows["wf-1"].Title = "Renamed in the web app"
		fx.wf.mu.Unlock()
	}, false)
	m := loadAnyManifest(t, fx.root)
	if m.Description == nil || *m.Description != "Edited in the web app." || m.UseDedicatedCompute == nil || !*m.UseDedicatedCompute {
		t.Fatalf("after pull description %v, useDedicatedCompute %v, want live's", m.Description, m.UseDedicatedCompute)
	}
	if m.FormatVersion < 10 {
		t.Errorf("a pulled setting switched on left formatVersion %d", m.FormatVersion)
	}
}

// A setting switched on below version 10 is refused at the acceptance point
// propose plans through, as sync apply does, and nothing is created.
func TestSyncRefusesAnUnderStampedSetting(t *testing.T) {
	f, dir := orderTree(t)
	root := adoptedFolder(t, f, dir, "workflows/a",
		&wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow a", Entrypoint: "main.py", UseDedicatedCompute: ptr(true)},
		nil, map[string]string{"main.py": "x\n"})
	declareFormatVersion(t, root, 6)
	want := `set "formatVersion": 10`
	report, _ := applyStack(t, dir, "dev", "--allow-create")
	if line := applyFolderLine(t, &report, "workflows/a"); line.Verdict != verdictRefused || !strings.Contains(line.Detail, want) {
		t.Errorf("sync apply line = %+v, want a refusal carrying %q", line, want)
	}
	if flows := workflowsIn(f, devFeature); len(flows) != 0 {
		t.Errorf("sync apply created %d workflows from an under-stamped folder", len(flows))
	}
}

// A folder `app init` wrote and pushed before its first propose holds a row
// no publish has made live, so its first publish plans as an update; the
// unit names that it is the first publish and every setting it takes live,
// since live has none to compare with.
func TestAFirstPublishNamesTheSettingsItSets(t *testing.T) {
	f, repo := setupFixture(t)
	if out, err := runCLI(t, repo, "app", "init", "new", "--feature", importFeature, "--title", "New"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	dir := filepath.Join(repo, "new")
	editFolderManifest(t, dir, wfdir.DataAppKind, func(m *wfdir.Manifest) {
		m.Description = ptr("Tracks loans.")
		m.SetServerDataOnly(true)
	})
	if out, err := runCLI(t, dir, "app", "push", "--no-validate"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	out, err := runPropose_(t, dir, "--json")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	_, folders := proposedDatabases(t, f)
	if len(folders) != 1 {
		t.Fatalf("plan folders %s, want one", folders)
	}
	var folder struct {
		Units []struct {
			Action string `json:"action"`
			Detail string `json:"detail"`
		} `json:"units"`
	}
	if err := json.Unmarshal(folders[0], &folder); err != nil {
		t.Fatal(err)
	}
	const want = "first publish; settings: description, server-side data only: on"
	if len(folder.Units) != 1 || folder.Units[0].Action != string(applyActionUpdate) || folder.Units[0].Detail != want {
		t.Fatalf("the first publish's unit: %s, want an update detailed %q", folders[0], want)
	}
}
