package commands

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// TestAppStatusRecommendedUpdates pins the one Remote line `app status` prints
// for pending app updates, and that nothing about them can fail the command.
// 403 (cannot draft the app) and 404 (an instance without the route) are
// answers that mean "nothing for you here", so they print nothing.
func TestAppStatusRecommendedUpdates(t *testing.T) {
	const updatesWord = "recommended update"
	cases := []struct {
		name    string
		answer  *fakeUpdatesAnswer
		want    string
		wantErr bool
	}{
		{
			name:   "several",
			answer: &fakeUpdatesAnswer{Count: 3},
			want:   "3 recommended updates (optional). See what to check: ronja api /api/v2/dataapp/data_app-1/updates · rebuild as a draft: ronja api -X POST /api/v2/dataapp/data_app-1/rebuild",
		},
		{
			name:   "one",
			answer: &fakeUpdatesAnswer{Count: 1},
			want:   "1 recommended update (optional). See what to check: ronja api /api/v2/dataapp/data_app-1/updates · rebuild as a draft: ronja api -X POST /api/v2/dataapp/data_app-1/rebuild",
		},
		{name: "none pending", answer: &fakeUpdatesAnswer{Count: 0}},
		{name: "not a drafter", answer: &fakeUpdatesAnswer{Status: http.StatusForbidden}},
		{name: "older instance", answer: nil},
		{name: "server error", answer: &fakeUpdatesAnswer{Status: http.StatusInternalServerError}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAppInstance(t)
			live := f.AddApp(&api.DataApp{ID: "data_app-1"}, api.DataAppFile{Path: "App.tsx", Content: "x"})
			signInApp(t, f)
			root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})
			f.updates = tc.answer

			out, err := runCLI(t, root, "app", "status")
			if err != nil {
				t.Fatalf("app status failed: %v", err)
			}
			if !strings.Contains(strings.Join(f.Requests, "\n"), "GET /api/v2/dataapp/data_app-1/updates") {
				t.Fatalf("status never asked for updates; requests were %v", f.Requests)
			}

			var mentions []string
			for line := range strings.Lines(out) {
				if strings.Contains(line, updatesWord) {
					mentions = append(mentions, strings.TrimSpace(line))
				}
			}
			switch {
			case tc.want != "":
				if len(mentions) != 1 || mentions[0] != tc.want {
					t.Errorf("want exactly the line\n  %s\ngot %q in:\n%s", tc.want, mentions, out)
				}
			case tc.wantErr:
				if len(mentions) != 1 || strings.Contains(mentions[0], "(optional)") {
					t.Errorf("want one line saying updates could not be checked, got %q in:\n%s", mentions, out)
				}
			default:
				if len(mentions) != 0 {
					t.Errorf("want no updates line, got %q in:\n%s", mentions, out)
				}
			}
		})
	}
}

// TestAppStatusJSONCarriesRecommendedUpdates: --json says what the person was
// told — the count when the server answered, the reason when it failed, and
// nothing when the answer was a 403 or 404.
func TestAppStatusJSONCarriesRecommendedUpdates(t *testing.T) {
	cases := []struct {
		name   string
		answer *fakeUpdatesAnswer
		check  func(t *testing.T, updates map[string]any, present bool)
	}{
		{
			name:   "count",
			answer: &fakeUpdatesAnswer{Count: 2},
			check: func(t *testing.T, u map[string]any, present bool) {
				if !present || u["checked"] != true || u["count"] != float64(2) {
					t.Errorf("want {checked:true, count:2}, got %v (present=%v)", u, present)
				}
			},
		},
		{
			name:   "server error",
			answer: &fakeUpdatesAnswer{Status: http.StatusInternalServerError},
			check: func(t *testing.T, u map[string]any, present bool) {
				reason, _ := u["notCheckedReason"].(string)
				if !present || u["checked"] != false || !strings.Contains(reason, "500") {
					t.Errorf("want {checked:false, notCheckedReason naming the 500}, got %v (present=%v)", u, present)
				}
			},
		},
		{
			name:   "not a drafter",
			answer: &fakeUpdatesAnswer{Status: http.StatusForbidden},
			check: func(t *testing.T, u map[string]any, present bool) {
				if present {
					t.Errorf("a 403 should leave updates out, got %v", u)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAppInstance(t)
			live := f.AddApp(&api.DataApp{ID: "data_app-1"}, api.DataAppFile{Path: "App.tsx", Content: "x"})
			signInApp(t, f)
			root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})
			f.updates = tc.answer

			out, err := runCLI(t, root, "app", "status", "--json")
			if err != nil {
				t.Fatalf("app status --json failed: %v", err)
			}
			var report struct {
				Remote map[string]json.RawMessage `json:"remote"`
			}
			decodeJSONInto(t, out, &report)
			raw, present := report.Remote["updates"]
			var updates map[string]any
			if present {
				if err := json.Unmarshal(raw, &updates); err != nil {
					t.Fatalf("remote.updates is not an object: %s", raw)
				}
			}
			tc.check(t, updates, present)
		})
	}
}

// TestSyncStatusDoesNotAskForRecommendedUpdates: `sync status` reuses the app
// status report but never prints its updates, so it must not pay a request per
// app folder for them.
func TestSyncStatusDoesNotAskForRecommendedUpdates(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "data_app-1"}, api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "private"
	signInApp(t, f)
	f.updates = &fakeUpdatesAnswer{Count: 2}

	tree := t.TempDir()
	root := filepath.Join(tree, "explorer")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAppFolder(t, root, &wfdir.Manifest{
		Kind: wfdir.KindDataApp, Title: "Revenue explorer",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
		},
	}, map[string]string{"App.tsx": "x"})
	lock := &wfdir.Lock{}
	lock.SetBinding("prod", wfdir.Binding{DataAppID: "data_app-1", FeatureID: "feat-1"})
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}

	out, _ := runCLI(t, tree, "sync", "status", "--stack", "prod", "--json")
	requests := strings.Join(f.Requests, "\n")
	if !strings.Contains(requests, "GET /api/v2/dataapp/data_app-1/files") {
		t.Fatalf("sync status never read the app's files, so this proves nothing; requests were %v\n%s", f.Requests, out)
	}
	if strings.Contains(requests, "/updates") {
		t.Errorf("sync status asked for recommended updates it never reports; requests were %v", f.Requests)
	}
}

// TestAppStatusSkipsUpdatesForAnUnpublishedApp: a binding that names a draft never
// published has no live app, so nothing is recommended and nothing is asked.
func TestAppStatusSkipsUpdatesForAnUnpublishedApp(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "data_app-new-1", Lifecycle: api.LifecycleDraft}, api.DataAppFile{Path: "App.tsx", Content: "x"})
	signInApp(t, f)
	f.updates = &fakeUpdatesAnswer{Count: 2}
	root := appFolderBoundTo(t, f, "data_app-new-1", map[string]string{"App.tsx": "x"})

	out, err := runCLI(t, root, "app", "status")
	if err != nil {
		t.Fatalf("app status failed: %v", err)
	}
	if strings.Contains(strings.Join(f.Requests, "\n"), "/updates") {
		t.Errorf("status asked for updates on an app never published; requests were %v", f.Requests)
	}
	if strings.Contains(out, "recommended update") {
		t.Errorf("want no updates line, got:\n%s", out)
	}
}
