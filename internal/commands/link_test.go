package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The closing link a sync loop prints, and the two rules it lives by.
//
// One file rather than four, because this is ONE behaviour spread across the
// commands: the link a report ends with is whatever the SERVER put on the
// response the command already holds, and nothing here ever templates a route
// or picks an origin.
//
// The tests come in pairs on purpose. The positive half pins that the link
// carries the FRONTEND origin (fakeFrontendOrigin), which the fake deliberately
// serves from a different host than its API — so a CLI that rebuilt the link
// from the profile's instance URL, the way this used to, fails here rather than
// in production where api.* and app.* differ and local dev where the backend
// serves no pages at all. The negative half pins that an instance which returns
// NO url produces no link, no warning and no failure: the field is omitempty
// server-side, an unconfigured frontend origin omits it, and that is the
// ordinary state of the dev box this gets tested on.

// urlLine returns the value of a report's "URL:" line, or "" when it printed
// none. Matched on the label rather than on a substring of the link, because
// the assertion that matters most is the ABSENCE of the line.
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
	// The DRAFT's page: a push writes to the draft, and that is the row whose
	// content the reader is being invited to look at.
	draftID := f.draftOf["wf-1"]
	if draftID == "" {
		t.Fatal("the push should have checked out a draft")
	}
	if got, want := urlLine(pushed), fakeFrontendOrigin+"/workflows/"+draftID; got != want {
		t.Errorf("push link = %q, want %q", got, want)
	}

	// status reports the WORKFLOW, whoever has a draft open — the row the
	// binding names, and the one a reader means by "the workflow". It is a
	// remote fact, so a status that never reached the server has none (see
	// TestNoServerLinkPrintsNothingAndStillSucceeds).
	status, err := runCLI(t, root, "wf", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got, want := urlLine(status), fakeFrontendOrigin+"/workflows/wf-1"; got != want {
		t.Errorf("status link = %q, want %q", got, want)
	}

	// A second push has nothing to do and still says where to look.
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

	// Publish reports the PARENT: the draft is gone the moment it commits, and
	// what the reader wants to open is what went live.
	published, err := runCLI(t, root, "wf", "publish")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got, want := urlLine(published), fakeFrontendOrigin+"/workflows/wf-1"; got != want {
		t.Errorf("publish link = %q, want %q", got, want)
	}
}

// The negative control, and the one that decides whether this is safe to ship:
// an instance with no frontend origin configured returns no url at all.
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
	// Not a warning either. A missing link is not a problem the person running
	// this can do anything about, and saying so on every push would train them
	// to ignore the notes that matter.
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

// `wf status`'s --json payload is unchanged by the link: its `url` key has
// always meant the INSTANCE origin, and the frontend page rides the human
// report only. A second url under another name would be a shape change to
// something scripts parse — and one document using `url` for two different
// origins is worse than no key at all.
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

// The --json payload is a contract for anything scripting the CLI. The link
// rides the human report only; a caller that wants it machine-readably reads it
// off the API response, which is where the CLI got it from.
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

	// status reports the APP, whoever has a draft open: it is the row the
	// binding names and the one a reader means by "the app".
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
	// And the LIVE link beside it: a draft is its own address, so the author
	// needs to be told which link keeps showing the published version.
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

// `app publish`'s link is the one that also rides the --json payload, as
// `appURL` — the field predates this work, so it stays published. What CHANGED
// is that it can now be absent: it used to be derived locally from the instance
// URL and was therefore always present, and always pointing at the API host.
//
// Both halves are pinned here, because the absence is only meaningful against a
// run that does emit the key: `jq -r .appURL` goes from a wrong string to
// `null`, and a test asserting only the absence would still pass if the field
// were dropped altogether.
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

// `app status` establishes the link from the REMOTE read, so a signed-out run —
// which never makes one — has nothing to report. It used to answer with a link
// assembled from the binding and the instance URL, which was a guess at both
// the origin and the row.
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
