package api

import (
	"context"
	"net/http"
	"testing"
)

// A realistic full response. The point of the fixture is TAG VERIFICATION: a
// field name that does not match backend/ronja/rdb/table_data_app.go decodes to
// the zero value silently, and a mirror nobody checks is a mirror that drifts.
//
// It matters more here than on the workflow row. A mistyped allowlist tag does
// not merely blank a column — the CLI would read the app's grants as empty and
// then PUSH them as empty, revoking the app's access to its own tables in the
// course of syncing a source file.
const dataAppFixture = `{
  "id": "data_app-abc",
  "workspaceID": null,
  "featureID": "feat-1",
  "name": "Revenue explorer",
  "description": "Last twelve months",
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
	// Normalize sorts, so the fixture's deliberately out-of-order tables come
	// back ordered — which is what makes SameAccess a comparison of meaning.
	if len(app.AllowedTableIDs) != 2 || app.AllowedTableIDs[0] != "table-1" {
		t.Errorf("allowedTableIDs = %v, want them sorted", app.AllowedTableIDs)
	}

	// Stamped by the SERVER (DataAppView) on a FRONTEND origin, which is not
	// the API origin this client was pointed at — the whole reason the CLI
	// reads it instead of templating one. Absent on an instance with no
	// configured frontend origin, which reads as "" and prints nothing.
	if app.URL != "https://app.example.test/apps/data_app-abc" {
		t.Errorf("url = %q", app.URL)
	}

	// IdentityID is what a manifest binding records: the parent, since a draft's
	// own id dies at commit.
	if app.IdentityID() != "data_app-parent" {
		t.Errorf("IdentityID() = %q", app.IdentityID())
	}
}

// TestGetDataAppDraftDecodesNullAsNoDraft pins the shape the route actually
// uses: 200 with a JSON `null` body, not a 404. Decoding that into a struct
// would turn "no draft" into a zero-value row with an empty id, and every caller
// would then act on a draft that does not exist.
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

// TestPutDataAppFileDecodesCompileError pins the response shape the backend fix
// introduced: a failed recompile arrives INSIDE a 200, alongside the saved file.
//
// A client that treated this as an error could not push a multi-file app at all,
// since the intermediate states do not compile by construction.
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
	// The write LANDED — that is the whole justification for the shape.
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

// TestSameAccessIgnoresOrderAndDuplicates: an allowlist is a SET. The server
// stores it as one and the token mint reads it as one, so a manifest listing two
// tables in the other order is the same grant — and treating it as drift would
// make `app status` report a change forever and `app push` send a patch that
// changes nothing.
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

// TestDataAppPatchSetAccessSetsEveryList pins that an access patch is all-or-
// nothing.
//
// Every field of the server's Patch is an optional: an absent key means
// "unchanged". So a patch that filled in only the lists a folder happens to use
// would leave the others pointing at whatever the row had — which is how an app
// keeps a secret nobody declared.
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

// TestDataAppFilePathEscapesSegments: the route is a gin wildcard, so the path's
// own slashes are part of it rather than something to encode away.
func TestDataAppFilePathEscapesSegments(t *testing.T) {
	got := dataAppFilePath("data_app-1", "lib/my chart.tsx")
	want := "dataapp/data_app-1/files/lib/my%20chart.tsx"
	if got != want {
		t.Errorf("dataAppFilePath = %q, want %q", got, want)
	}
}
