package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serve points a client at a one-route stub. Unlike newTestClient this needs no
// poll-timing surgery — these calls are single round trips.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-token")
}

// A realistic full response. The point of the fixture is TAG VERIFICATION: a
// field name that does not match backend/ronja/rdb/table_workflow.go decodes to
// the zero value silently, and a mirror nobody checks is a mirror that drifts.
const workflowFixture = `{
  "id": "wf-abc",
  "workspaceID": null,
  "featureID": "feat-1",
  "title": "Monthly report",
  "description": "Rolls up last month",
  "filenameTemplate": "",
  "entrypoint": "main.py",
  "kind": "report",
  "parameters": [
    {"name": "month", "label": "Month", "type": "select", "required": true,
     "options": ["jan", "feb"], "optionsQuery": "SELECT m FROM {{ ref('tbl-1') }}",
     "defaultValue": "jan", "description": "which month"}
  ],
  "variables": {"threshold": 10},
  "useDedicatedCompute": true,
  "approvalGate": {"enabled": true, "contextParamNames": ["month"],
                   "approvers": ["user-1"], "authorMessage": "please check"},
  "parentWorkflowID": "wf-parent",
  "shadowStatus": "draft",
  "committedAt": null,
  "hidden": true,
  "private": false,
  "secretIDs": ["sec-1"],
  "explicitSecretIDs": ["sec-2"],
  "inputTableIDs": ["tbl-1"],
  "outputTableIDs": ["tbl-2"],
  "agentIDs": ["agent-1"],
  "codexIDs": ["cdx-1"],
  "querySecretIDs": ["sec-3"],
  "pipPackages": ["requests==2.31.0"],
  "tags": [],
  "scope": "personal",
  "featureScope": "organization",
  "authorUserID": "user-1",
  "lifecycle": "draft",
  "archivedAt": null,
  "submittedForReviewAt": "2026-07-28T09:00:00Z",
  "drafterUserID": "user-1",
  "drafterKind": "user",
  "createdAt": "2026-07-01T08:00:00Z",
  "updatedAt": "2026-07-28T10:30:00Z"
}`

func TestGetWorkflowDecodesEveryMirroredField(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/workflow/wf-abc" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q", got)
		}
		w.Write([]byte(workflowFixture))
	})

	wf, err := client.GetWorkflow(context.Background(), "wf-abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"ID", wf.ID, "wf-abc"},
		{"FeatureID", wf.FeatureID, "feat-1"},
		{"Title", wf.Title, "Monthly report"},
		{"Description", wf.Description, "Rolls up last month"},
		{"Entrypoint", wf.Entrypoint, "main.py"},
		{"Kind", wf.Kind, "report"},
		{"UseDedicatedCompute", wf.UseDedicatedCompute, true},
		{"ParentWorkflowID", wf.ParentWorkflowID, "wf-parent"},
		{"Hidden", wf.Hidden, true},
		{"Lifecycle", wf.Lifecycle, "draft"},
		{"DrafterUserID", wf.DrafterUserID, "user-1"},
		{"FeatureScope", wf.FeatureScope, "organization"},
		{"len(Parameters)", len(wf.Parameters), 1},
		{"Parameters[0].Name", wf.Parameters[0].Name, "month"},
		{"Parameters[0].Label", wf.Parameters[0].Label, "Month"},
		{"Parameters[0].Type", wf.Parameters[0].Type, "select"},
		{"Parameters[0].Required", wf.Parameters[0].Required, true},
		{"Parameters[0].Description", wf.Parameters[0].Description, "which month"},
		{"Parameters[0].OptionsQuery", wf.Parameters[0].OptionsQuery, "SELECT m FROM {{ ref('tbl-1') }}"},
		{"SecretIDs[0]", wf.SecretIDs[0], "sec-1"},
		{"ExplicitSecretIDs[0]", wf.ExplicitSecretIDs[0], "sec-2"},
		{"InputTableIDs[0]", wf.InputTableIDs[0], "tbl-1"},
		{"OutputTableIDs[0]", wf.OutputTableIDs[0], "tbl-2"},
		{"AgentIDs[0]", wf.AgentIDs[0], "agent-1"},
		{"CodexIDs[0]", wf.CodexIDs[0], "cdx-1"},
		{"QuerySecretIDs[0]", wf.QuerySecretIDs[0], "sec-3"},
		{"PipPackages[0]", wf.PipPackages[0], "requests==2.31.0"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if wf.Variables["threshold"] != float64(10) {
		t.Errorf("Variables = %v", wf.Variables)
	}
	if !wf.IsGated() {
		t.Errorf("approval gate = %+v", wf.ApprovalGate)
	}
	if wf.SubmittedForReviewAt == nil || wf.SubmittedForReviewAt.Year() != 2026 {
		t.Errorf("SubmittedForReviewAt = %v", wf.SubmittedForReviewAt)
	}
	if wf.CommittedAt != nil {
		t.Errorf("a null timestamp must decode to nil, got %v", wf.CommittedAt)
	}
	if wf.UpdatedAt.IsZero() || wf.CreatedAt.IsZero() {
		t.Errorf("timestamps did not decode: created=%v updated=%v", wf.CreatedAt, wf.UpdatedAt)
	}
	// optional.V[string] marshals to `null` when unset; unmarshalling null into
	// a string is a documented no-op, and "" is exactly what unset means here.
	if wf.WorkspaceID != "" {
		t.Errorf("WorkspaceID = %q, want empty", wf.WorkspaceID)
	}
	// A draft's binding must be the PARENT — the draft id dies at commit.
	if wf.IdentityID() != "wf-parent" {
		t.Errorf("IdentityID = %q, want wf-parent", wf.IdentityID())
	}
}

func TestGetWorkflowNormalizesAbsentSlices(t *testing.T) {
	// Empty ID slices carry omitempty server-side, so they arrive ABSENT rather
	// than as []. Code that ranges or compares should not have to care.
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"wf-1","lifecycle":"live","approvalGate":null}`))
	})
	wf, err := client.GetWorkflow(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wf.OutputTableIDs == nil || len(wf.OutputTableIDs) != 0 {
		t.Errorf("OutputTableIDs = %v, want empty non-nil", wf.OutputTableIDs)
	}
	if wf.SecretIDs == nil || wf.AgentIDs == nil || wf.CodexIDs == nil ||
		wf.QuerySecretIDs == nil || wf.InputTableIDs == nil ||
		wf.ExplicitSecretIDs == nil || wf.PipPackages == nil || wf.Parameters == nil {
		t.Error("every absent slice must normalize to empty")
	}
	if wf.IsGated() {
		t.Error("a null approvalGate must not read as gated")
	}
	// Parentless: the row is its own stable identity.
	if wf.IdentityID() != "wf-1" {
		t.Errorf("IdentityID = %q, want wf-1", wf.IdentityID())
	}
}

// The draft route answers 200 with a JSON `null` BODY when the caller has no
// draft — not a 404. Decoding that into a struct would manufacture a
// zero-valued draft with an empty id, and every caller would then act on a
// draft that does not exist.
func TestGetWorkflowDraftNullBodyIsNil(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/workflow/wf-1/draft" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte("null"))
	})
	draft, err := client.GetWorkflowDraft(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft != nil {
		t.Fatalf("expected nil for a null body, got %+v", draft)
	}
}

func TestGetWorkflowDraftPresent(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"wf-draft","parentWorkflowID":"wf-1","lifecycle":"draft"}`))
	})
	draft, err := client.GetWorkflowDraft(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft == nil || draft.ID != "wf-draft" || draft.Lifecycle != LifecycleDraft {
		t.Fatalf("draft = %+v", draft)
	}
	if draft.SecretIDs == nil {
		t.Error("a present draft must be normalized too")
	}
}

func TestListWorkflowFiles(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/workflow/wf-1/files" {
			t.Errorf("path = %s", r.URL.Path)
		}
		// A bare array, not an envelope — the route returns []*rdb.WorkflowFile.
		w.Write([]byte(`[
          {"id":"wff-1","workflowID":"wf-1","path":"main.py","content":"print(1)",
           "createdAt":"2026-07-01T08:00:00Z","updatedAt":"2026-07-28T10:00:00Z"},
          {"id":"wff-2","workflowID":"wf-1","path":"lib/helpers.py","content":"x = 1",
           "createdAt":"2026-07-01T08:00:00Z","updatedAt":"2026-07-01T08:00:00Z"}
        ]`))
	})
	files, err := client.ListWorkflowFiles(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if files[0].Path != "main.py" || files[0].Content != "print(1)" || files[0].ID != "wff-1" {
		t.Errorf("file[0] = %+v", files[0])
	}
	if files[1].Path != "lib/helpers.py" || files[1].WorkflowID != "wf-1" {
		t.Errorf("file[1] = %+v", files[1])
	}
	if files[0].UpdatedAt.IsZero() || files[0].CreatedAt.IsZero() {
		t.Errorf("timestamps did not decode: %+v", files[0])
	}
}

func TestGetWorkflowNotFound(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`))
	})
	_, err := client.GetWorkflow(context.Background(), "wf-missing")
	if StatusOf(err) != 404 {
		t.Fatalf("status = %d, want 404 (err %v)", StatusOf(err), err)
	}
}
