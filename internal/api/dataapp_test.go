package api

import (
	"context"
	"net/http"
	"testing"
)

// The fixture is TAG VERIFICATION against backend/ronja/rdb/table_data_app.go.
// A mistyped allowlist tag reads the app's grants as empty and then PUSHES them
// as empty, revoking its access to its own tables.
const dataAppFixture = `{
  "id": "data_app-abc",
  "workspaceID": null,
  "featureID": "feat-1",
  "name": "Revenue explorer",
  "description": "Last twelve months",
  "specVersion": 2,
  "entrypoint": "App.tsx",
  "bundleFileKey": "dataapp/file-1.html",
  "bundleCompiledAt": "2026-08-04T09:00:00Z",
  "validatedAt": "2026-08-04T09:00:01Z",
  "allowedSecretIDs": ["secret-1"],
  "allowedTableIDs": ["table-2", "table-1"],
  "allowedCodexIDs": ["codex-1"],
  "allowedAgentIDs": ["agent-1"],
  "allowedWorkflowIDs": ["workflow-1"],
  "allowedMetricIDs": ["table-metric-1"],
  "capabilities": ["query_ronja"],
  "reportingTimezone": "Europe/Stockholm",
  "scope": "personal",
  "authorUserID": "user-1",
  "lifecycle": "draft",
  "archivedAt": null,
  "parentDataAppID": "data_app-parent",
  "committedAt": null,
  "baseVersionID": null,
  "drafterUserID": "user-1",
  "drafterKind": "user",
  "proposedAt": null,
  "submittedForReviewAt": "2026-08-04T09:30:00Z",
  "whoCanSuggest": "everyone",
  "markedForDeletionAt": null,
  "createdBy": "user-1",
  "createdAt": "2026-08-01T08:00:00Z",
  "updatedAt": "2026-08-04T10:30:00Z",
  "url": "https://app.example.test/apps/data_app-abc"
}`

func TestGetDataAppDecodesEveryMirroredField(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/dataapp/data_app-abc" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dataAppFixture))
	})

	app, err := client.GetDataApp(context.Background(), "data_app-abc")
	if err != nil {
		t.Fatalf("GetDataApp: %v", err)
	}

	if app.ID != "data_app-abc" || app.FeatureID != "feat-1" {
		t.Errorf("identity fields: %+v", app)
	}
	if app.Name != "Revenue explorer" {
		t.Errorf("name = %q", app.Name)
	}
	if app.Entrypoint != "App.tsx" {
		t.Errorf("entrypoint = %q", app.Entrypoint)
	}
	// A mistyped `specVersion` decodes to 0: "this app styles itself".
	if app.SpecVersion != 2 {
		t.Errorf("specVersion = %d, want 2 (a mistyped tag decodes silently to zero)", app.SpecVersion)
	}
	if app.BundleFileKey != "dataapp/file-1.html" {
		t.Errorf("bundleFileKey = %q", app.BundleFileKey)
	}
	if !app.IsValidated() {
		t.Error("validatedAt should have decoded")
	}
	if app.Lifecycle != LifecycleDraft {
		t.Errorf("lifecycle = %q", app.Lifecycle)
	}
	if app.ParentDataAppID != "data_app-parent" {
		t.Errorf("parentDataAppID = %q", app.ParentDataAppID)
	}
	if app.SubmittedForReviewAt == nil {
		t.Error("submittedForReviewAt should have decoded")
	}
	if app.DrafterUserID != "user-1" || app.CreatedBy != "user-1" {
		t.Errorf("attribution fields: %+v", app)
	}

	// Every allowlist, individually: this is the set where a silent zero value
	// becomes a revoked capability on the next push.
	for _, tc := range []struct {
		label string
		got   []string
		want  string
	}{
		{"allowedSecretIDs", app.AllowedSecretIDs, "secret-1"},
		{"allowedCodexIDs", app.AllowedCodexIDs, "codex-1"},
		{"allowedAgentIDs", app.AllowedAgentIDs, "agent-1"},
		{"allowedWorkflowIDs", app.AllowedWorkflowIDs, "workflow-1"},
		{"allowedMetricIDs", app.AllowedMetricIDs, "table-metric-1"},
		{"capabilities", app.Capabilities, "query_ronja"},
	} {
		if len(tc.got) != 1 || tc.got[0] != tc.want {
			t.Errorf("%s = %v, want [%s]", tc.label, tc.got, tc.want)
		}
	}
	// The fixture's tables are deliberately out of order; Normalize sorts.
	if len(app.AllowedTableIDs) != 2 || app.AllowedTableIDs[0] != "table-1" {
		t.Errorf("allowedTableIDs = %v, want them sorted", app.AllowedTableIDs)
	}

	// Stamped by the SERVER on the FRONTEND origin, which is not the API origin
	// this client knows, so the CLI reads it rather than templating one.
	if app.URL != "https://app.example.test/apps/data_app-abc" {
		t.Errorf("url = %q", app.URL)
	}

	// IdentityID is what a manifest binding records: the parent, since a draft's
	// own id dies at commit.
	if app.IdentityID() != "data_app-parent" {
		t.Errorf("IdentityID() = %q", app.IdentityID())
	}
}

// "No draft" is 200 with a JSON `null` body; decoded into a struct it would be a
// zero-value draft every caller then acts on.
func TestGetDataAppDraftDecodesNullAsNoDraft(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("null"))
	})
	draft, err := client.GetDataAppDraft(context.Background(), "data_app-abc")
	if err != nil {
		t.Fatalf("GetDataAppDraft: %v", err)
	}
	if draft != nil {
		t.Errorf("a null body must decode to no draft, got %+v", draft)
	}
}

// A failed recompile arrives INSIDE a 200 beside the saved file: a multi-file
// push's intermediate states do not compile by construction.
func TestPutDataAppFileDecodesCompileError(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "id": "dataappfile-1", "dataAppID": "data_app-draft-1", "path": "App.tsx",
		  "content": "broken", "createdAt": "2026-08-04T10:00:00Z",
		  "updatedAt": "2026-08-04T10:00:00Z",
		  "compileError": {
		    "message": "data app compile failed: App.tsx:2:0: Unexpected end of file",
		    "diagnostics": [{"message": "Unexpected end of file", "line": 2, "column": 0, "file": "App.tsx"}]
		  }
		}`))
	})

	saved, err := client.PutDataAppFile(context.Background(), "data_app-1", "App.tsx", "broken", nil)
	if err != nil {
		t.Fatalf("a compile failure must not be a request failure: %v", err)
	}
	// The write LANDED.
	if saved.Content != "broken" || saved.DataAppID != "data_app-draft-1" {
		t.Errorf("the saved file should come back: %+v", saved.DataAppFile)
	}
	if saved.CompileError == nil {
		t.Fatal("expected compileError to decode")
	}
	if len(saved.CompileError.Diagnostics) != 1 || saved.CompileError.Diagnostics[0].Line != 2 {
		t.Errorf("structured diagnostics not carried through: %+v", saved.CompileError)
	}
}

// An allowlist is a SET on the server; reading order as drift would make `app
// status` report a change forever.
func TestSameAccessIgnoresOrderAndDuplicates(t *testing.T) {
	a := DataAppAccess{AllowedTableIDs: []string{"table-b", "table-a"}}
	b := DataAppAccess{AllowedTableIDs: []string{"table-a", "table-b", "table-a"}}
	if !SameAccess(a, b) {
		t.Errorf("reordered and duplicated ids are the same grant: %v vs %v", a, b)
	}
	c := DataAppAccess{AllowedTableIDs: []string{"table-a"}}
	if SameAccess(a, c) {
		t.Error("a genuinely different set must not compare equal")
	}
	// A nil list and an empty one both mean "grants nothing", and a folder must
	// not see one as drift against the other.
	if !SameAccess(DataAppAccess{}, DataAppAccess{AllowedTableIDs: []string{}}) {
		t.Error("nil and empty must compare equal")
	}
}

// An absent Patch key means "unchanged", so a patch filling only the lists a
// folder uses would let an app keep a secret nobody declared.
func TestDataAppPatchSetAccessSetsEveryList(t *testing.T) {
	var patch DataAppPatch
	patch.SetAccess(DataAppAccess{AllowedTableIDs: []string{"table-1"}})
	for label, slot := range map[string]*[]string{
		"allowedTableIDs":    patch.AllowedTableIDs,
		"allowedSecretIDs":   patch.AllowedSecretIDs,
		"allowedAgentIDs":    patch.AllowedAgentIDs,
		"allowedWorkflowIDs": patch.AllowedWorkflowIDs,
		"allowedCodexIDs":    patch.AllowedCodexIDs,
		"allowedMetricIDs":   patch.AllowedMetricIDs,
		"capabilities":       patch.Capabilities,
	} {
		if slot == nil {
			t.Errorf("%s must be set explicitly, not left absent", label)
		}
	}
	if patch.Empty() {
		t.Error("a patch that sets the allowlists is not empty")
	}
}

// The route is a gin wildcard, so the path's own slashes stay unencoded.
func TestDataAppFilePathEscapesSegments(t *testing.T) {
	got := dataAppFilePath("data_app-1", "lib/my chart.tsx")
	want := "dataapp/data_app-1/files/lib/my%20chart.tsx"
	if got != want {
		t.Errorf("dataAppFilePath = %q, want %q", got, want)
	}
}
