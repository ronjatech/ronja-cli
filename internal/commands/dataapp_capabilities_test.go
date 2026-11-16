package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The server's refusal of a capability name it does not know
// (rdataapp.NormalizeCapabilities), as the fake answers it.
const serverUnknownCapabilityRefusal = `"upload_fil" is not a data-app capability. An app declares only "ai", "embed", "query_external", "upload_file", "write_external"; the automatic ones (call_function, codex, http_fetch, list_users, query_metric, query_ronja, resource_link, run_agent, run_workflow) come from the app's allowlists or are on every app, and are dropped if sent`

func queryRonjaNote(root string) string {
	return "Note: " + wfdir.ManifestPath(root) + ` lists "query_ronja" under access.capabilities; Ronja grants it on its own, from the app's allowlists or to every app, so it is ignored. Delete it from the file.`
}

// storedCapabilityLists are what a row can hold: a clean list, and the list a
// row Ronja's agent built held before the server dropped automatic names.
var storedCapabilityLists = []struct {
	name   string
	stored []string
}{
	{"clean", []string{"ai"}},
	{"with an automatic name", []string{"ai", "query_ronja"}},
}

// appWithStoredCapabilities adds live app-1 and makes it serve exactly
// `stored`, bypassing AddApp's normalization so the wire carries the raw list.
func appWithStoredCapabilities(t *testing.T, f *fakeAppInstance, stored []string) *api.DataApp {
	t.Helper()
	app := f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	app.Capabilities = slices.Clone(stored)
	f.versions["app-1"] = []api.DataApp{{ID: "dav-1"}}
	f.featureScope["feat-1"] = "private"
	return app
}

// cloneAppForCapabilities clones app-1 onto stack prod and returns the folder
// root and the clone's --json document.
func cloneAppForCapabilities(t *testing.T) (string, map[string]any) {
	t.Helper()
	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	doc := decodeJSON(t, out)
	return doc["root"].(string), doc
}

// rawCapabilitiesOf reads access.capabilities from ronja.json as written,
// without any normalized view.
func rawCapabilitiesOf(t *testing.T, root string) []string {
	t.Helper()
	var file struct {
		Access *struct {
			Capabilities []string `json:"capabilities"`
		} `json:"access"`
	}
	if err := json.Unmarshal([]byte(readFile(t, root, wfdir.ManifestName)), &file); err != nil {
		t.Fatalf("decode %s: %v", wfdir.ManifestName, err)
	}
	if file.Access == nil {
		t.Fatalf("%s has no access block", wfdir.ManifestName)
	}
	return file.Access.Capabilities
}

// writeRawCapabilities writes capabilities into ronja.json the way an older
// CLI did, without SetAccess's normalization.
func writeRawCapabilities(t *testing.T, root string, capabilities []string) {
	t.Helper()
	m := appManifestOf(t, root)
	access := m.DeclaredAccess()
	access.Capabilities = capabilities
	m.Access = &access
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

// appStatusAccessChanges runs `app status --json` and returns its access
// changes and stderr.
func appStatusAccessChanges(t *testing.T, root string) ([]accessChange, string) {
	t.Helper()
	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, root, "app", "status", "--json")
	})
	if err != nil {
		t.Fatalf("app status: %v\n%s", err, out)
	}
	var report appStatusReport
	decodeJSONInto(t, out, &report)
	if report.Remote == nil || !report.Remote.Checked {
		t.Fatalf("status did not check the remote: %s", out)
	}
	return report.Remote.AccessChanges, stderr
}

func pushApp(t *testing.T, root string) (string, error) {
	t.Helper()
	var err error
	stderr := captureStderr(t, func() {
		_, err = runCLI(t, root, "app", "push")
	})
	return stderr, err
}

// TestAppCloneThenPushRoundTripsTheStoredCapabilities: a clone writes only the
// declarable names, its --json says the same, status is clean, and an
// unchanged push sends no update.
func TestAppCloneThenPushRoundTripsTheStoredCapabilities(t *testing.T) {
	for _, tc := range storedCapabilityLists {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAppInstance(t)
			appWithStoredCapabilities(t, f, tc.stored)
			signInApp(t, f)

			root, doc := cloneAppForCapabilities(t)
			access, _ := doc["access"].(map[string]any)
			if got, _ := json.Marshal(access["capabilities"]); string(got) != `["ai"]` {
				t.Errorf("clone --json access.capabilities = %s, want [\"ai\"]", got)
			}
			if got := rawCapabilitiesOf(t, root); !slices.Equal(got, []string{"ai"}) {
				t.Errorf("the clone wrote capabilities %v, want [ai]", got)
			}
			if changes, _ := appStatusAccessChanges(t, root); len(changes) != 0 {
				t.Errorf("status reports an access change on a fresh clone: %+v", changes)
			}
			if _, err := pushApp(t, root); err != nil {
				t.Fatalf("an unchanged push failed: %v", err)
			}
			if len(f.patches) != 0 {
				t.Errorf("an unchanged push sent %d data-app updates: %+v", len(f.patches), f.patches)
			}
		})
	}
}

// TestImportThenApplyRoundTripsTheStoredCapabilities is the same round trip
// through `ronja import feature`, whose tree is checked and deployed with
// `sync status` and `sync apply` (TestImportFeatureProof).
func TestImportThenApplyRoundTripsTheStoredCapabilities(t *testing.T) {
	for _, tc := range storedCapabilityLists {
		t.Run(tc.name, func(t *testing.T) {
			f := importFixture(t)
			f.app.apps["data_app-dash"].Capabilities = slices.Clone(tc.stored)

			dir, _ := runImport(t, f)
			if got := rawCapabilitiesOf(t, filepath.Join(dir, "apps", "sales-dashboard")); !slices.Equal(got, []string{"ai"}) {
				t.Errorf("the import wrote capabilities %v, want [ai]", got)
			}
			status, err := runCLI(t, dir, "sync", "status", "--stack", "main", "--json")
			if err != nil || decodeJSON(t, status)["verdict"] != verdictClean {
				t.Fatalf("sync status of a fresh import is not clean: %v\n%s", err, status)
			}
			f.resetRequests()
			apply, err := runCLI(t, dir, "sync", "apply", "--stack", "main", "--json")
			if err != nil {
				t.Fatalf("sync apply: %v\n%s", err, apply)
			}
			if len(f.app.patches) != 0 {
				t.Errorf("an unchanged apply sent %d data-app updates: %+v", len(f.app.patches), f.app.patches)
			}
			if writes := f.writes(); len(writes) != 0 {
				t.Errorf("an unchanged apply sent %v", writes)
			}
		})
	}
}

// TestAppPushIgnoresAnAutomaticCapabilityAnOlderCLIWrote: the folder still
// lists query_ronja. Push succeeds without an update and says to delete the
// name, once; status says it too.
func TestAppPushIgnoresAnAutomaticCapabilityAnOlderCLIWrote(t *testing.T) {
	f := newFakeAppInstance(t)
	appWithStoredCapabilities(t, f, []string{"ai"})
	signInApp(t, f)
	root, _ := cloneAppForCapabilities(t)
	writeRawCapabilities(t, root, []string{"ai", "query_ronja"})

	stderr, err := pushApp(t, root)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if n := strings.Count(stderr, queryRonjaNote(root)); n != 1 {
		t.Errorf("push printed the note %d times, want once:\n%s", n, stderr)
	}
	if len(f.patches) != 0 {
		t.Errorf("push sent %d data-app updates for a name the server never stores: %+v", len(f.patches), f.patches)
	}

	changes, stderr := appStatusAccessChanges(t, root)
	if len(changes) != 0 {
		t.Errorf("status reports an access change: %+v", changes)
	}
	if n := strings.Count(stderr, queryRonjaNote(root)); n != 1 {
		t.Errorf("status printed the note %d times, want once:\n%s", n, stderr)
	}
}

// TestAppPushSendsARealCapabilityChange: adding upload_file is sent, and the
// automatic name beside it is not.
func TestAppPushSendsARealCapabilityChange(t *testing.T) {
	f := newFakeAppInstance(t)
	appWithStoredCapabilities(t, f, []string{"ai"})
	signInApp(t, f)
	root, _ := cloneAppForCapabilities(t)
	writeRawCapabilities(t, root, []string{"ai", "query_ronja", "upload_file"})

	if _, err := pushApp(t, root); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.patches) != 1 || f.patches[0].Capabilities == nil {
		t.Fatalf("want one update carrying the capabilities, got %+v", f.patches)
	}
	if got := *f.patches[0].Capabilities; !slices.Equal(got, []string{"ai", "upload_file"}) {
		t.Errorf("the update sent capabilities %v, want [ai upload_file]", got)
	}
}

// TestAppPullDropsAnAutomaticCapabilityWhenItRewritesAccess: live gained a
// table, so pull rewrites the access block of a folder that listed
// query_ronja. The new block holds the table and not query_ronja, and pull
// says once that it dropped the name, not that the author should.
func TestAppPullDropsAnAutomaticCapabilityWhenItRewritesAccess(t *testing.T) {
	fx := appIdempotenceFixture(t)
	f := fx.app
	f.apps["app-1"].Capabilities = []string{"ai"}
	inCleanGit(t)
	writeRawCapabilities(t, fx.root, []string{"ai", "query_ronja"})
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}

	f.apps["app-1"].AllowedTableIDs = []string{"table-new"}
	fx.draftKind.webPublish("fixed in the web app\n")
	if err := os.RemoveAll(filepath.Join(fx.root, wfdir.StateDirName)); err != nil {
		t.Fatal(err)
	}

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, fx.root, "app", "pull", "--json")
	})
	if err != nil {
		t.Fatalf("pull: %v\n%s\n%s", err, out, stderr)
	}
	m := appManifestOf(t, fx.root)
	if got := m.Access.AllowedTableIDs; !slices.Equal(got, []string{"table-new"}) {
		t.Errorf("pull wrote tables %v, want [table-new]", got)
	}
	if got := rawCapabilitiesOf(t, fx.root); !slices.Equal(got, []string{"ai"}) {
		t.Errorf("pull wrote capabilities %v, want [ai]", got)
	}
	want := "Note: " + wfdir.ManifestPath(fx.root) + ` listed "query_ronja" under access.capabilities; Ronja grants it on its own, from the app's allowlists or to every app, so pull dropped it from the file.`
	if n := strings.Count(stderr, want); n != 1 {
		t.Errorf("pull printed the note %d times, want once:\n%s", n, stderr)
	}
	if strings.Contains(stderr, "Delete it from the file") {
		t.Errorf("pull asked the author to delete a name it already dropped:\n%s", stderr)
	}
}

// TestAppPushShowsTheServersRefusalOfAnUnknownCapability: the CLI sends a
// name it does not know, and the server's refusal reaches the author intact
// before any file is written, on both the access-patch and the create path.
func TestAppPushShowsTheServersRefusalOfAnUnknownCapability(t *testing.T) {
	assertRefusedBeforeAnyFile := func(t *testing.T, f *fakeAppInstance, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected the push to stop on the server's refusal")
		}
		if !strings.Contains(err.Error(), serverUnknownCapabilityRefusal) {
			t.Errorf("the push did not show the server's refusal intact, got:\n%v", err)
		}
		if len(f.fileWrites) != 0 {
			t.Errorf("files were written before the refusal: %+v", f.fileWrites)
		}
		for _, request := range f.Requests {
			if strings.Contains(request, "/files/") && !strings.HasPrefix(request, "GET ") {
				t.Errorf("a file request was sent before the refusal: %s", request)
			}
		}
	}

	t.Run("access patch", func(t *testing.T) {
		f := newFakeAppInstance(t)
		appWithStoredCapabilities(t, f, []string{"ai"})
		signInApp(t, f)
		root, _ := cloneAppForCapabilities(t)
		writeRawCapabilities(t, root, []string{"ai", "upload_fil"})
		writeLocal(t, root, "App.tsx", "changed\n")
		f.failPatch, f.failPatchMessage = 400, serverUnknownCapabilityRefusal

		_, err := pushApp(t, root)
		assertRefusedBeforeAnyFile(t, f, err)
		if len(f.patches) != 1 || f.patches[0].Capabilities == nil || !slices.Contains(*f.patches[0].Capabilities, "upload_fil") {
			t.Errorf("the unknown name was not sent: %+v", f.patches)
		}
	})

	t.Run("create", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
			map[string]string{"App.tsx": "x"})
		m := appManifestOf(t, root)
		m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
		m.Access = &api.DataAppAccess{Capabilities: []string{"ai", "upload_fil"}}
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
		f.failCreate, f.failCreateMessage = 400, serverUnknownCapabilityRefusal

		_, err := pushApp(t, root)
		assertRefusedBeforeAnyFile(t, f, err)
		if len(f.created) != 1 || !slices.Contains(f.created[0].Capabilities, "upload_fil") {
			t.Errorf("the unknown name was not sent: %+v", f.created)
		}
	})
}
