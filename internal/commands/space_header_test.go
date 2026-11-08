package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// spaceSeenMatching is the requests that carried X-Ronja-Space, as
// "METHOD path space", whose "METHOD path" starts with prefix.
func spaceSeenMatching(f *fakeInstance, prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, seen := range f.spaceSeen {
		if strings.HasPrefix(seen, prefix) {
			out = append(out, seen)
		}
	}
	return out
}

// addMainStack gives a Dev folder a "main" stack on the Main feature, which
// records no Space.
func addMainStack(t *testing.T, f *fakeInstance, root string) {
	t.Helper()
	m := loadAnyManifest(t, root)
	m.Stacks["main"] = wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

// A Dev stack's every request carries its Space; a Main stack's, and a folder
// with no stack, carry none.
func TestADevStacksRequestsCarryItsSpace(t *testing.T) {
	f := spacedInstance(t)
	root := devStackFolder(t, f, "", nil, "result = 1\n")
	if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err != nil {
		t.Fatalf("push --stack dev: %v", err)
	}
	writes := 0
	for _, req := range f.Requests {
		if !strings.HasPrefix(req, "GET ") {
			writes++
			if len(spaceSeenMatching(f, req)) == 0 {
				t.Errorf("%s went out without the stack's Space", req)
			}
		}
	}
	if writes == 0 {
		t.Fatalf("the push wrote nothing: %v", f.Requests)
	}
	for _, seen := range spaceSeenMatching(f, "") {
		if !strings.HasSuffix(seen, " space-dev") {
			t.Errorf("a request named another Space: %s", seen)
		}
	}

	addMainStack(t, f, root)
	f.spaceSeen = nil
	if _, err := runCLI(t, root, "wf", "push", "--stack", "main"); err != nil {
		t.Fatalf("push --stack main: %v", err)
	}
	if len(f.spaceSeen) != 0 {
		t.Fatalf("a Main stack sent a Space: %v", f.spaceSeen)
	}

	plain := t.TempDir()
	if _, err := runCLI(t, plain, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init on Main: %v", err)
	}
	writeLocal(t, plain, "main.py", "result = 2\n")
	if _, err := runCLI(t, plain, "wf", "push"); err != nil {
		t.Fatalf("push with no stack: %v", err)
	}
	if len(f.spaceSeen) != 0 {
		t.Fatalf("a folder with no stack sent a Space: %v", f.spaceSeen)
	}
}

// A tree command sends each folder its own stack's Space.
func TestSyncApplySendsEachFolderItsOwnStacksSpace(t *testing.T) {
	f := spacedInstance(t)
	f.featureSpace["feat-qa"] = "space-qa"
	f.spaceNames["space-qa"] = "QA"
	tree := t.TempDir()
	for _, folder := range []struct{ dir, feature string }{{"dev-report", "feat-dev"}, {"qa-report", "feat-qa"}} {
		root := filepath.Join(tree, folder.dir)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := runCLI(t, root, "wf", "init", "--feature", folder.feature, "--stack", "dev"); err != nil {
			t.Fatalf("init %s: %v", folder.dir, err)
		}
		writeLocal(t, root, "main.py", "result = 1\n")
	}
	f.spaceSeen = nil
	if _, err := runCLI(t, tree, "sync", "apply", "--stack", "dev", "--allow-create"); err != nil {
		t.Fatalf("sync apply --stack dev: %v", err)
	}
	for _, want := range []string{"GET /api/v2/feature/feat-dev space-dev", "GET /api/v2/feature/feat-qa space-qa"} {
		if len(spaceSeenMatching(f, want)) == 0 {
			t.Errorf("no %q among %v", want, f.spaceSeen)
		}
	}
	for _, wrong := range []string{"GET /api/v2/feature/feat-dev space-qa", "GET /api/v2/feature/feat-qa space-dev"} {
		if got := spaceSeenMatching(f, wrong); len(got) != 0 {
			t.Errorf("a folder sent the other folder's Space: %v", got)
		}
	}
}

// An instance that does not echo the Space, a person outside the Space and an
// organization API token are each refused by name on the first response that
// carried the Space, at exit 2, and nothing further goes out with it; `sync
// check` and `sync apply` report the folder under the same reason.
func TestASpaceTheCLICannotActInIsRefused(t *testing.T) {
	for _, tc := range []struct {
		mode, reason string
		want, never  []string
	}{
		{"ignored", syncReasonSpaceHeaderUnsupported, []string{
			"did not confirm it acted in Space Dev (space-dev)",
			"does not honour the Space header", "Nothing further was sent",
		}, nil},
		{"unavailable", syncReasonSpaceUnavailable, []string{
			"cannot enter Space Dev (space-dev)", "`ronja login`",
		}, nil},
		{"not_selectable", syncReasonSpaceNotSelectable, []string{
			"organization API token", "API Tokens page", "Personal token", "`ronja login`", "personal access token",
		}, []string{"RONJA_TOKEN"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := spacedInstance(t)
			root := devStackFolder(t, f, "", nil, "result = 1\n")
			f.spaceHeader = tc.mode
			f.Requests, f.spaceSeen = nil, nil

			_, err := runCLI(t, root, "wf", "push", "--stack", "dev")
			e, ok := asSpaceEntry(err)
			if !ok || e.Reason != tc.reason {
				t.Fatalf("push: %v, want %s", err, tc.reason)
			}
			for _, want := range tc.want {
				if !strings.Contains(e.Detail, want) {
					t.Errorf("refusal %q does not say %q", e.Detail, want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(e.Detail, never) {
					t.Errorf("refusal %q says %q", e.Detail, never)
				}
			}
			if exitCodeOf(err) != syncExitUnknown {
				t.Errorf("exit code: %v", err)
			}
			if got := spaceSeenMatching(f, ""); len(got) != 1 {
				t.Errorf("sent with the Space after the refusal: %v", got)
			}
			if len(requestsMatching(f, "PUT ")) != 0 || len(requestsMatching(f, "POST ")) != 0 || len(f.created) != 0 {
				t.Errorf("wrote anyway: %v", f.Requests)
			}

			for _, args := range [][]string{{"sync", "check"}, {"sync", "apply"}} {
				report, err := runCLI(t, filepath.Dir(root), append(args, "--stack", "dev", "--json")...)
				if !strings.Contains(report, `"reason": "`+tc.reason+`"`) && !strings.Contains(report, `"reason":"`+tc.reason+`"`) {
					t.Errorf("%v did not report %s: %v\n%s", args, tc.reason, err, report)
				}
				if exitCodeOf(err) != syncExitUnknown {
					t.Errorf("%v exit: %v", args, err)
				}
			}
		})
	}
}

// sentWithoutSpace is the requests of requests that did not carry
// X-Ronja-Space, given spaceSeen (as fakeInstance records it), beyond /me and
// the Space lookup's by-id reads of a feature and its Space.
func sentWithoutSpace(requests, spaceSeen []string) []string {
	carried := map[string]int{}
	for _, seen := range spaceSeen {
		carried[seen[:strings.LastIndex(seen, " ")]]++
	}
	var out []string
	for _, req := range requests {
		if carried[req] > 0 {
			carried[req]--
			continue
		}
		feature, isFeature := strings.CutPrefix(req, "GET /api/v2/feature/")
		if req == "GET /api/v2/authentication/me" || strings.HasPrefix(req, "GET /api/v2/space/") || (isFeature && !strings.Contains(feature, "/")) {
			continue
		}
		out = append(out, req)
	}
	return out
}

// Every workflow and module command on a Dev stack sends its Space on every
// request but the organization's /me and the Space lookup's.
func TestEveryDevStackCommandCarriesItsSpace(t *testing.T) {
	f := spacedInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	withFastPolling(t)
	wf := devStackFolder(t, f, "", nil, "result = 1\n")
	mod := t.TempDir()
	if _, err := runCLI(t, mod, "module", "init", "dev_tracker", "--feature", "feat-dev", "--stack", "dev", "--title", "Dev tracker"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	writeLocal(t, mod, "tracker.py", "x = 1\n")

	for _, step := range []struct {
		root string
		edit string
		args []string
	}{
		{wf, "", []string{"wf", "push"}},
		{wf, "", []string{"wf", "publish"}},
		{wf, "main.py", []string{"wf", "push"}},
		{wf, "", []string{"wf", "status"}},
		{wf, "", []string{"wf", "validate"}},
		{wf, "", []string{"wf", "test"}},
		{wf, "", []string{"wf", "discard", "--yes"}},
		{mod, "", []string{"module", "push"}},
		{mod, "", []string{"module", "publish"}},
		{mod, "tracker.py", []string{"module", "push"}},
		{mod, "", []string{"module", "status"}},
		{mod, "", []string{"module", "discard", "--yes"}},
	} {
		if step.edit != "" {
			writeLocal(t, step.root, step.edit, "# edited\nx = 2\n")
		}
		f.mu.Lock()
		f.Requests, f.spaceSeen = nil, nil
		f.mu.Unlock()
		args := append(step.args, "--stack", "dev")
		if out, err := runCLI(t, step.root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		f.mu.Lock()
		requests, seen := f.Requests, f.spaceSeen
		f.mu.Unlock()
		if len(seen) == 0 {
			t.Errorf("%v sent nothing with the Space: %v", args, requests)
		}
		if bare := sentWithoutSpace(requests, seen); len(bare) != 0 {
			t.Errorf("%v sent without the stack's Space: %v", args, bare)
		}
		for _, s := range seen {
			if !strings.HasSuffix(s, " space-dev") {
				t.Errorf("%v named another Space: %s", args, s)
			}
		}
	}
}

// Every data app command on a Dev stack sends its Space on every request but
// the organization's /me and the Space lookup's.
func TestEveryDevStackAppCommandCarriesItsSpace(t *testing.T) {
	f := newFakeAppInstance(t)
	f.featureSpace = map[string]string{"feat-dev": "space-dev"}
	f.spaceNames = map[string]string{"space-dev": "Dev"}
	signInApp(t, f)
	f.AnswerPreviewResult(api.PreviewResult{Rendered: true, Viewport: "desktop",
		Errors: []api.PreviewRuntimeError{}, ConsoleErrors: []api.PreviewConsoleError{}})
	root := t.TempDir()
	if _, err := runCLI(t, root, "app", "init", "--feature", "feat-dev", "--stack", "dev"); err != nil {
		t.Fatalf("app init: %v", err)
	}

	for _, step := range []struct {
		edit bool
		args []string
	}{
		{false, []string{"app", "push"}},
		{false, []string{"app", "publish"}},
		{true, []string{"app", "push"}},
		{false, []string{"app", "status"}},
		{false, []string{"app", "test", "--out-dir", t.TempDir()}},
		{false, []string{"app", "discard", "--yes"}},
	} {
		if step.edit {
			writeLocal(t, root, "App.tsx", "export default function App() { return null }\n")
		}
		f.Requests, f.spaceSeen = nil, nil
		args := append(step.args, "--stack", "dev")
		if out, err := runCLI(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if len(f.spaceSeen) == 0 {
			t.Errorf("%v sent nothing with the Space: %v", args, f.Requests)
		}
		if bare := sentWithoutSpace(f.Requests, f.spaceSeen); len(bare) != 0 {
			t.Errorf("%v sent without the stack's Space: %v", args, bare)
		}
		for _, s := range f.spaceSeen {
			if !strings.HasSuffix(s, " space-dev") {
				t.Errorf("%v named another Space: %s", args, s)
			}
		}
	}
}

// A Dev stack's client with no token sends no Space: the request fails as the
// server's own sign-in refusal.
func TestATokenlessStackClientSendsNoSpace(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get(api.HeaderSpace))
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	key := wfdir.InstanceKey{URL: srv.URL, TenantID: testTenantID}
	stack := wfdir.Stack{URL: srv.URL, TenantID: testTenantID, FeatureID: "feat-dev", SpaceID: "space-dev"}
	client := stackClient(&config.Resolved{URL: srv.URL}, key, stack)

	_, err := client.GetWorkflow(t.Context(), "wf-1")
	if api.StatusOf(err) != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the read's own 401", err)
	}
	if len(seen) != 1 || seen[0] != "GET /api/v2/workflow/wf-1 " {
		t.Fatalf("requests = %q, want the one read with no Space", seen)
	}
}

// On an instance that does not echo the Space, the command stops on the first
// response that carried it, though the Space lookup that read it swallows its
// own failures, and nothing is sent after it.
func TestNothingIsSentAfterAnUnconfirmedSpace(t *testing.T) {
	f := spacedInstance(t)
	root := devStackFolder(t, f, "", nil, "result = 1\n")
	f.spaceHeader = "ignored"
	f.mu.Lock()
	f.Requests, f.spaceSeen = nil, nil
	f.mu.Unlock()

	_, err := runCLI(t, root, "wf", "push", "--stack", "dev")
	if e, ok := asSpaceEntry(err); !ok || e.Reason != syncReasonSpaceHeaderUnsupported || exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("push: %v (exit %d), want %s at exit 2", err, exitCodeOf(err), syncReasonSpaceHeaderUnsupported)
	}
	if seen := spaceSeenMatching(f, ""); len(seen) != 1 || seen[0] != "GET /api/v2/space/space-dev space-dev" {
		t.Fatalf("carried the Space: %v, want only the Space lookup's read", seen)
	}
	f.mu.Lock()
	requests := f.Requests
	f.mu.Unlock()
	if len(requests) == 0 || requests[len(requests)-1] != "GET /api/v2/space/space-dev" {
		t.Fatalf("sent after the unconfirmed answer: %v", requests)
	}
}

// A 404 without the echo is no answer about the row: an instance that did not
// act in the Space may have looked for it elsewhere. The push stops on the
// draft lookup's at exit 2 and writes nothing, not even the draft it would
// otherwise open.
func TestAnUnconfirmedNotFoundStopsAPushBeforeAnyWrite(t *testing.T) {
	f := spacedInstance(t)
	root := devStackFolder(t, f, "", nil, "result = 1\n")
	for _, args := range [][]string{{"wf", "push"}, {"wf", "publish"}} {
		if out, err := runCLI(t, root, append(args, "--stack", "dev")...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if len(f.workflows) != 1 {
		t.Fatalf("want one workflow, got %v", f.workflows)
	}
	for id := range f.workflows {
		f.failDraft = http.StatusNotFound
		f.spaceEchoMissing = map[string]bool{"GET /api/v2/workflow/" + id + "/draft": true}
	}
	writeLocal(t, root, "main.py", "result = 2\n")
	f.mu.Lock()
	f.Requests, f.spaceSeen = nil, nil
	f.mu.Unlock()

	_, err := runCLI(t, root, "wf", "push", "--stack", "dev")
	if e, ok := asSpaceEntry(err); !ok || e.Reason != syncReasonSpaceHeaderUnsupported || !strings.Contains(e.Detail, "HTTP 404") || exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("push on an unconfirmed 404: %v (exit %d)", err, exitCodeOf(err))
	}
	if writes := append(requestsMatching(f, "POST "), append(requestsMatching(f, "PUT "), requestsMatching(f, "PATCH ")...)...); len(writes) != 0 {
		t.Fatalf("wrote after an unconfirmed 404: %v", writes)
	}
}

// A status whose Space requests get no answer degrades like any unanswered
// read: the local half is reported. A definite refusal of an adopted folder's
// state read is the command's answer, by its reason at exit 2.
func TestAStatusOnADevStackDegradesOnlyWhenUnanswered(t *testing.T) {
	f := spacedInstance(t)
	root := devStackFolder(t, f, "", nil, "result = 1\n")
	if _, err := runCLI(t, root, "wf", "push", "--stack", "dev"); err != nil {
		t.Fatalf("push: %v", err)
	}
	writeLocal(t, root, "main.py", "result = 2\n")
	f.spaceHeader = "unanswered"
	out, err := runCLI(t, root, "wf", "status", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("status on an unanswering instance: %v\n%s", err, out)
	}
	var report struct {
		Local  wfdir.Diff `json:"local"`
		Remote struct {
			Checked bool `json:"checked"`
		} `json:"remote"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if len(report.Local.Modified) != 1 || report.Local.Modified[0] != "main.py" || report.Remote.Checked {
		t.Fatalf("want the local half and an unchecked remote, got %s", out)
	}

	f.spaceHeader = ""
	if out, err := runCLI(t, filepath.Dir(root), "sync", "adopt", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	f.spaceHeader = "not_selectable"
	_, err = runCLI(t, root, "wf", "status", "--stack", "dev", "--json")
	if e, ok := asSpaceEntry(err); !ok || e.Reason != syncReasonSpaceNotSelectable || exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("status of an adopted folder an organization token reads: %v (exit %d)", err, exitCodeOf(err))
	}
}

// `ronja api` sends an X-Ronja-Space the caller wrote as written and checks
// nothing about the answer: a raw request is the caller's own.
func TestARawRequestNamingASpaceIsNotChecked(t *testing.T) {
	f := spacedInstance(t)
	f.spaceHeader = "ignored"
	if out, err := runCLI(t, t.TempDir(), "api", "-H", "X-Ronja-Space: space-dev", "/api/v2/feature/feat-dev"); err != nil {
		t.Fatalf("ronja api -H: %v\n%s", err, out)
	}
	if seen := spaceSeenMatching(f, "GET /api/v2/feature/feat-dev"); len(seen) != 1 {
		t.Fatalf("want the one request with the caller's header, got %v", seen)
	}
}

// A validate answer that does not confirm the Space is the Space's refusal, not
// a verdict on the folder: skipping the check would not change it, so the push
// does not advise --no-validate.
func TestAnUnconfirmedValidateDoesNotAdviseSkippingIt(t *testing.T) {
	f := spacedInstance(t)
	root := devStackFolder(t, f, "", nil, "result = 1\n")
	f.spaceEchoMissing = map[string]bool{"POST /api/v2/workflow/validate": true}

	_, err := runCLI(t, root, "wf", "push", "--stack", "dev")
	if e, ok := asSpaceEntry(err); !ok || e.Reason != syncReasonSpaceHeaderUnsupported || !strings.Contains(e.Detail, "POST /api/v2/workflow/validate") || exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("push on an unconfirmed validate: %v (exit %d)", err, exitCodeOf(err))
	}
	if strings.Contains(err.Error(), "--no-validate") {
		t.Errorf("the Space refusal advises skipping validation: %v", err)
	}
}
