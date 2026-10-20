package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// A report's closing link is whatever url the server put on the response; the
// CLI never templates a route or picks an origin. The fake serves its frontend
// origin from a different host than its API, so a link rebuilt from the
// instance URL fails here. A missing url (no frontend origin configured) prints
// no link, no warning and no failure.

// urlLine matches on the label, not the link, because the assertion that
// matters most is the line's absence.
func urlLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "URL:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func TestPushAndPublishReportTheServersLink(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")

	pushed, err := runCLI(t, root, "wf", "push")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	// A push links the draft: that is the row it wrote.
	draftID := f.draftOf["wf-1"]
	if draftID == "" {
		t.Fatal("the push should have checked out a draft")
	}
	if got, want := urlLine(pushed), fakeFrontendOrigin+"/workflows/"+draftID; got != want {
		t.Errorf("push link = %q, want %q", got, want)
	}

	// status links the workflow the binding names, whoever has a draft open.
	status, err := runCLI(t, root, "wf", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got, want := urlLine(status), fakeFrontendOrigin+"/workflows/wf-1"; got != want {
		t.Errorf("status link = %q, want %q", got, want)
	}

	upToDate, err := runCLI(t, root, "wf", "push")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if !strings.Contains(upToDate, "Up to date") {
		t.Fatalf("expected an up-to-date report, got:\n%s", upToDate)
	}
	if got := urlLine(upToDate); got == "" {
		t.Errorf("an up-to-date push should still report the link, got none:\n%s", upToDate)
	}

	// Publish links the parent: the draft is gone once it commits.
	published, err := runCLI(t, root, "wf", "publish")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got, want := urlLine(published), fakeFrontendOrigin+"/workflows/wf-1"; got != want {
		t.Errorf("publish link = %q, want %q", got, want)
	}
}

// The negative control: an instance with no frontend origin returns no url.
func TestNoServerLinkPrintsNothingAndStillSucceeds(t *testing.T) {
	f := newFakeInstance(t)
	f.noFrontendOrigin = true
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")

	pushed, err := runCLI(t, root, "wf", "push")
	if err != nil {
		t.Fatalf("push must succeed without a link to print: %v", err)
	}
	if got := urlLine(pushed); got != "" {
		t.Errorf("no url from the server must print no link, got %q", got)
	}
	// Not a warning either: the reader can do nothing about it, and a note on
	// every push trains them to ignore notes.
	if strings.Contains(strings.ToLower(pushed), "url") {
		t.Errorf("the report mentions a URL it does not have:\n%s", pushed)
	}

	status, err := runCLI(t, root, "wf", "status")
	if err != nil {
		t.Fatalf("status must succeed without a link to print: %v", err)
	}
	if got := urlLine(status); got != "" {
		t.Errorf("no url from the server must print no link, got %q", got)
	}

	published, err := runCLI(t, root, "wf", "publish")
	if err != nil {
		t.Fatalf("publish must succeed without a link to print: %v", err)
	}
	if got := urlLine(published); got != "" {
		t.Errorf("no url from the server must print no link, got %q", got)
	}
}

// `url` in `wf status --json` means the instance origin; the frontend link
// rides the human report only, so scripts see no shape change.
func TestStatusJSONKeepsURLMeaningTheInstance(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	payload := decodeJSON(t, out)
	if got := payload["url"]; got != f.URL() {
		t.Errorf("status --json url = %v, want the instance %q", got, f.URL())
	}
	for _, key := range []string{"workflowURL", "appURL", "pageURL"} {
		if _, present := payload[key]; present {
			t.Errorf("wf status --json grew a %q key: %v", key, payload)
		}
	}
	if strings.Contains(out, fakeFrontendOrigin) {
		t.Errorf("the link must not reach the --json payload:\n%s", out)
	}
}

// The link rides the human report only; a script reads it off the API
// response.
func TestPushJSONCarriesNoURLKey(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	payload := decodeJSON(t, out)
	for _, key := range []string{"url", "URL"} {
		if _, present := payload[key]; present {
			t.Errorf("wf push --json grew a %q key: %v", key, payload)
		}
	}
	if strings.Contains(out, fakeFrontendOrigin) {
		t.Errorf("the link must not be double-printed under --json:\n%s", out)
	}
}

func TestAppCommandsReportTheServersLink(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	draft := f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "private"
	signInApp(t, f)
	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})

	// status links the app the binding names, whoever has a draft open.
	status, err := runCLI(t, root, "app", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got, want := urlLine(status), fakeFrontendOrigin+"/apps/"+live.ID; got != want {
		t.Errorf("status link = %q, want %q", got, want)
	}

	writeLocal(t, root, "App.tsx", "y")
	pushed, err := runCLI(t, root, "app", "push")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if got, want := urlLine(pushed), fakeFrontendOrigin+"/apps/"+draft.ID; got != want {
		t.Errorf("push link = %q, want %q", got, want)
	}
	// A draft is its own address, so the report also names the live link.
	if !strings.Contains(pushed, "Live URL:") || !strings.Contains(pushed, fakeFrontendOrigin+"/apps/"+live.ID+"  (unchanged") {
		t.Errorf("push report does not name the unchanged live link:\n%s", pushed)
	}

	published, err := runCLI(t, root, "app", "publish")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got, want := urlLine(published), fakeFrontendOrigin+"/apps/"+live.ID; got != want {
		t.Errorf("publish link = %q, want %q", got, want)
	}
}

func TestAppCommandsWithoutAServerLinkPrintNothing(t *testing.T) {
	f := newFakeAppInstance(t)
	f.noFrontendOrigin = true
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "private"
	signInApp(t, f)
	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})

	writeLocal(t, root, "App.tsx", "y")
	for _, args := range [][]string{{"app", "status"}, {"app", "push"}, {"app", "publish"}} {
		out, err := runCLI(t, root, args...)
		if err != nil {
			t.Fatalf("%v must succeed without a link to print: %v", args, err)
		}
		if got := urlLine(out); got != "" {
			t.Errorf("%v printed %q with no url from the server", args, got)
		}
	}
}

// `app publish --json` is the one payload that carries the link, as `appURL`,
// and it may be absent. Both halves are pinned: an absence-only test would pass
// if the field were dropped altogether.
func TestAppPublishJSONCarriesTheServersLinkOrNoKey(t *testing.T) {
	publishJSON := func(t *testing.T, noFrontendOrigin bool) map[string]any {
		t.Helper()
		f := newFakeAppInstance(t)
		f.noFrontendOrigin = noFrontendOrigin
		live := f.AddApp(&api.DataApp{ID: "data_app-1"})
		f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
		f.featureScope["feat-1"] = "private"
		signInApp(t, f)
		root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})

		out, err := runCLI(t, root, "app", "publish", "--json")
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if noFrontendOrigin && strings.Contains(out, fakeFrontendOrigin) {
			t.Errorf("an instance with no frontend origin cannot produce a link:\n%s", out)
		}
		return decodeJSON(t, out)
	}

	t.Run("configured", func(t *testing.T) {
		payload := publishJSON(t, false)
		if got, want := payload["appURL"], fakeFrontendOrigin+"/apps/data_app-1"; got != want {
			t.Errorf("appURL = %v, want %q", got, want)
		}
	})

	t.Run("unconfigured", func(t *testing.T) {
		payload := publishJSON(t, true)
		if got, present := payload["appURL"]; present {
			t.Errorf("appURL must be OMITTED when the server sends no url, got %v", got)
		}
	})
}

// The link comes from the remote read, which a signed-out run never makes.
func TestAppStatusSignedOutReportsNoLink(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	signInApp(t, f)
	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})
	signOutFrom(t, f.URL())

	out, err := runCLI(t, root, "app", "status")
	if err != nil {
		t.Fatalf("status must still work signed out: %v", err)
	}
	if got := urlLine(out); got != "" {
		t.Errorf("a signed-out status cannot know the link, got %q", got)
	}
	if !strings.Contains(out, "not signed in") {
		t.Errorf("expected the remote half to say why it was not checked:\n%s", out)
	}
}
