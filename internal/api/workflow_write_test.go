package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Same discipline as workflow_test.go: the fixtures are TAG VERIFICATION
// against the server structs (rworkflow.ValidateResult, the workflow handler's
// FileSaveResponse / FileDeleteResponse), because a name that does not match
// decodes to the zero value in silence — and here that silence means "no
// findings" on a broken folder, or a lost warning.

const validateFixture = `{
  "findings": [
    {"severity": "error", "code": "unresolved_ref",
     "message": "\"tbl-gone\" isn't a table reachable from any of your workspaces",
     "path": "main.py", "marker": "{{ ref('tbl-gone') }}"},
    {"severity": "warning", "code": "secret_dropped",
     "message": "secret \"sec-1\" isn't reachable to you", "path": "lib/helpers.py"}
  ],
  "resolved": {
    "inputTableIDs": ["tbl-1"], "outputTableIDs": [], "secretIDs": ["sec-2"],
    "querySecretIDs": [], "agentIDs": ["agent-1"], "codexIDs": []
  }
}`

func TestValidateWorkflowFilesDecodesFindings(t *testing.T) {
	var sent ValidateInput
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/workflow/validate" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("request body: %v", err)
		}
		w.Write([]byte(validateFixture))
	})

	result, err := client.ValidateWorkflowFiles(context.Background(), ValidateInput{
		FeatureID:  "feat-1",
		Entrypoint: "main.py",
		Files:      []ValidateFile{{Path: "main.py", Content: "x"}},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if sent.FeatureID != "feat-1" || len(sent.Files) != 1 || sent.Files[0].Path != "main.py" {
		t.Errorf("request = %+v", sent)
	}

	if len(result.Findings) != 2 {
		t.Fatalf("findings = %+v", result.Findings)
	}
	first := result.Findings[0]
	if !first.IsError() || first.Code != "unresolved_ref" ||
		first.Path != "main.py" || first.Marker != "{{ ref('tbl-gone') }}" || first.Message == "" {
		t.Errorf("first finding = %+v", first)
	}
	if result.OK() {
		t.Error("OK() is true with an error finding")
	}
	if errs := result.Errors(); len(errs) != 1 {
		t.Errorf("Errors() = %+v", errs)
	}
	if len(result.Resolved.InputTableIDs) != 1 || len(result.Resolved.AgentIDs) != 1 {
		t.Errorf("resolved = %+v", result.Resolved)
	}
}

// Warnings alone are not a failure — a save succeeds through them, and so does
// a push.
func TestValidateOKWithWarningsOnly(t *testing.T) {
	result := &ValidateResult{Findings: []ValidateFinding{
		{Severity: "warning", Code: "secret_dropped"},
	}}
	if !result.OK() {
		t.Error("OK() = false with only warnings")
	}
}

func TestPutWorkflowFileSendsContentAndReadsWarnings(t *testing.T) {
	var gotPath, gotMethod string
	var sent struct {
		Content string `json:"content"`
	}
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)
		w.Write([]byte(`{"id":"file-1","workflowID":"wf-1","path":"lib/helpers.py",
		  "content":"X = 1","createdAt":"2026-07-28T09:00:00Z","updatedAt":"2026-07-28T09:00:00Z",
		  "warnings":["secret sec-1 was filtered out"]}`))
	})

	saved, err := client.PutWorkflowFile(context.Background(), "wf-1", "lib/helpers.py", "X = 1", nil)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if gotMethod != "PUT" || gotPath != "/api/v2/workflow/wf-1/files/lib/helpers.py" {
		t.Errorf("%s %s — the path's own slashes must survive escaping", gotMethod, gotPath)
	}
	if sent.Content != "X = 1" {
		t.Errorf("sent content = %q", sent.Content)
	}
	// The embedded file is at the response ROOT, not under a key.
	if saved.Path != "lib/helpers.py" || saved.Content != "X = 1" || saved.WorkflowID != "wf-1" {
		t.Errorf("saved = %+v", saved.WorkflowFile)
	}
	if len(saved.Warnings) != 1 {
		t.Errorf("warnings = %+v", saved.Warnings)
	}
}

// GetWorkflowFile exists for one caller — the push, reconciling a write whose
// outcome it does not know — so what is worth pinning is that it asks the
// single-file route rather than the collection, and that a path the row does not
// hold comes back as an error rather than as an empty file.
func TestGetWorkflowFileReadsOneFile(t *testing.T) {
	var gotMethod, gotPath string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Write([]byte(`{"id":"file-1","workflowID":"wf-1","path":"lib/helpers.py","content":"X = 1"}`))
	})
	file, err := client.GetWorkflowFile(context.Background(), "wf-1", "lib/helpers.py")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/api/v2/workflow/wf-1/files/lib/helpers.py" {
		t.Errorf("%s %s", gotMethod, gotPath)
	}
	if file.Content != "X = 1" {
		t.Errorf("file = %+v", file)
	}

	missing := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	})
	if _, err := missing.GetWorkflowFile(context.Background(), "wf-1", "gone.py"); err == nil {
		t.Fatal("a file the row does not hold came back clean")
	} else if StatusOf(err) != 404 {
		t.Errorf("status = %d, want 404", StatusOf(err))
	}
}

// The two halves of the timeout model, on the call that needs both.
//
// A per-request deadline may only ever SHORTEN the caller's — the client's own
// timeout is the ceiling, not the policy — and what came back has to be
// distinguishable from a refusal, because the push treats them differently: a
// rejected write certainly did not land, while a timed-out one may have.
func TestPutHonoursAShorterCallerDeadlineAndReportsItAsATimeout(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := client.PutWorkflowFile(ctx, "wf-1", "main.py", "x", nil); err == nil {
		t.Fatal("a request that outlived its deadline came back clean")
	} else if !IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false — the caller cannot tell a deadline from a refusal", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("took %v — the caller's own deadline was not applied", elapsed)
	}
}

// And a refusal is not a timeout: the server considered the write and said no,
// so the file certainly kept its previous content.
func TestIsTimeoutIgnoresAServerRefusal(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusBadRequest)
	})
	_, err := client.PutWorkflowFile(context.Background(), "wf-1", "main.py", "x", nil)
	if err == nil {
		t.Fatal("a 400 came back clean")
	}
	if IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = true for an HTTP 400", err)
	}
}

func TestDeleteWorkflowFileReadsWarnings(t *testing.T) {
	var gotMethod, gotPath string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Write([]byte(`{"warnings":["the last reference to secret sec-1 is gone"]}`))
	})
	deleted, err := client.DeleteWorkflowFile(context.Background(), "wf-1", "lib/old.py", nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/api/v2/workflow/wf-1/files/lib/old.py" {
		t.Errorf("%s %s", gotMethod, gotPath)
	}
	if len(deleted.Warnings) != 1 {
		t.Errorf("warnings = %+v", deleted.Warnings)
	}
}

// The four lifecycle verbs share a shape: empty body in, nothing out. What is
// worth pinning is that each hits its own route — and that request-review lives
// under the GOVERNANCE prefix, not beside the others.
func TestLifecycleVerbsHitTheirRoutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Client) error
		want string
	}{
		{"checkout", func(c *Client) error {
			_, err := c.CheckoutWorkflow(context.Background(), "wf-1")
			return err
		}, "/api/v2/workflow/wf-1/checkout"},
		{"commit", func(c *Client) error {
			return c.CommitWorkflowDraft(context.Background(), "draft-1", "")
		}, "/api/v2/workflow/draft-1/commit"},
		{"publish", func(c *Client) error {
			return c.PublishWorkflowDraft(context.Background(), "draft-1")
		}, "/api/v2/workflow/draft-1/publish"},
		{"discard", func(c *Client) error {
			return c.DiscardWorkflowDraft(context.Background(), "draft-1")
		}, "/api/v2/workflow/draft-1/discard"},
		{"request-review", func(c *Client) error {
			return c.RequestWorkflowReview(context.Background(), "draft-1")
		}, "/api/v2/workflow/draft/draft-1/request-review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			client := serve(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
				if r.Method != "POST" {
					t.Errorf("method = %s", r.Method)
				}
				w.Write([]byte(`{"id":"draft-1"}`))
			})
			if err := tc.call(client); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("path = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCreateAndUpdateWorkflow(t *testing.T) {
	var created CreateWorkflowInput
	var patches []string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.Method {
		case "POST":
			_ = json.Unmarshal(body, &created)
			w.Write([]byte(`{"id":"wf-new","lifecycle":"draft","title":"Monthly report"}`))
		case "PUT":
			patches = append(patches, string(body))
			w.Write([]byte(`null`))
		}
	})

	wf, err := client.CreateWorkflow(context.Background(), CreateWorkflowInput{
		FeatureID: "feat-1", Title: "Monthly report", Entrypoint: "main.py",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.FeatureID != "feat-1" || created.Entrypoint != "main.py" {
		t.Errorf("create body = %+v", created)
	}
	if wf.ID != "wf-new" || wf.Lifecycle != LifecycleDraft {
		t.Errorf("created = %+v", wf)
	}
	// Normalized: a created row carries no bindings at all, and callers should
	// not have to nil-check each one.
	if wf.InputTableIDs == nil || wf.OutputTableIDs == nil {
		t.Error("binding slices were not normalized")
	}

	if err := client.UpdateWorkflow(context.Background(), "wf-new", WorkflowPatch{Title: "New name"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := client.UpdateWorkflow(context.Background(), "wf-new", WorkflowPatch{Entrypoint: "run.py"}); err != nil {
		t.Fatalf("entrypoint patch: %v", err)
	}
	if err := client.UpdateWorkflow(context.Background(), "wf-new", WorkflowPatch{}); err != nil {
		t.Fatalf("empty patch: %v", err)
	}
	// Each field OMITTED when there is none: every field of the server's Patch
	// is an optional, so an explicit "" would CLEAR the value rather than leave
	// it alone.
	want := []string{`{"title":"New name"}`, `{"entrypoint":"run.py"}`, `{}`}
	if len(patches) != 3 || patches[0] != want[0] || patches[1] != want[1] || patches[2] != want[2] {
		t.Errorf("patch bodies = %q, want %q", patches, want)
	}
	if !(WorkflowPatch{}).Empty() || (WorkflowPatch{Entrypoint: "run.py"}).Empty() {
		t.Error("Empty() does not describe the patch it is asked about")
	}
}

// --- optimistic concurrency --------------------------------------------------

// The precondition's THREE states have to survive the encoder distinctly, and
// the raw body is what is asserted rather than a decoded struct: the whole
// hazard is that a *string collapsing to a string makes "no precondition" and
// "must not exist" the same bytes, and no decode of the sender's own type can
// see that.
func TestPutWorkflowFileEncodesThePreconditionsThreeStates(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	empty := ""
	for _, tc := range []struct {
		name string
		want string
		base *string
	}{
		{"absent", `{"content":"X = 1"}`, nil},
		{"must not exist", `{"content":"X = 1","baseSha256":""}`, &empty},
		{"digest", `{"content":"X = 1","baseSha256":"` + digest + `"}`, &digest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			client := serve(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got = string(body)
				w.Write([]byte(`{"id":"file-1","workflowID":"wf-1","path":"lib/helpers.py","content":"X = 1"}`))
			})
			if _, err := client.PutWorkflowFile(context.Background(), "wf-1", "lib/helpers.py", "X = 1", tc.base); err != nil {
				t.Fatalf("put: %v", err)
			}
			if got != tc.want {
				t.Errorf("body = %s, want %s", got, tc.want)
			}
		})
	}
}

// The two routes whose body is OPTIONAL send genuinely nothing when they have
// nothing to say. That is not cosmetic: the previous shape of both was bodyless,
// and `{}` is a different request for a server that distinguishes an absent
// field from a zero one.
func TestOptionalBodiesAreOmittedEntirely(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		call func(*Client) error
		want string
	}{
		{"delete without a precondition", func(c *Client) error {
			_, err := c.DeleteWorkflowFile(context.Background(), "wf-1", "old.py", nil)
			return err
		}, ""},
		{"delete with one", func(c *Client) error {
			_, err := c.DeleteWorkflowFile(context.Background(), "wf-1", "old.py", &digest)
			return err
		}, `{"baseSha256":"` + digest + `"}`},
		{"commit without a confirmation", func(c *Client) error {
			return c.CommitWorkflowDraft(context.Background(), "draft-1", "")
		}, ""},
		{"commit with one", func(c *Client) error {
			return c.CommitWorkflowDraft(context.Background(), "draft-1", "ver-9")
		}, `{"confirmHeadVersionID":"ver-9"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			client := serve(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got = string(body)
				w.Write([]byte(`{}`))
			})
			if err := tc.call(client); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

// The head is element ZERO of the versions listing, and an EMPTY listing means
// the workflow has never been versioned — in which case the live row itself is
// the anchor rworkflow.resolveHeadVersionID falls back to. Getting the second
// half wrong sends "" as a confirmation, which the server reads as "no
// override" and refuses all over again.
func TestHeadVersionIDReadsElementZeroAndFallsBackToTheWorkflowID(t *testing.T) {
	var gotPath string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`[{"id":"ver-3","committedAt":"2026-07-28T12:00:00Z"},
		                {"id":"ver-2","committedAt":"2026-07-28T11:00:00Z"}]`))
	})
	head, err := client.HeadVersionID(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if gotPath != "/api/v2/workflow/wf-1/versions" {
		t.Errorf("path = %s", gotPath)
	}
	if head != "ver-3" {
		t.Errorf("head = %s, want the newest version ver-3", head)
	}

	unversioned := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	head, err = unversioned.HeadVersionID(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("head of an unversioned workflow: %v", err)
	}
	if head != "wf-1" {
		t.Errorf("head = %q, want the workflow's own id — an unversioned workflow IS its own anchor", head)
	}
}

// StatusConflict is a wire contract in the same sense the lifecycle strings
// are: two commands branch on it, and one of them must keep it out of a
// fallback keyed on 400.
func TestConflictIsReportedAsItsOwnStatus(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"main.py changed since you last read it"}`, StatusConflict)
	})
	_, err := client.PutWorkflowFile(context.Background(), "wf-1", "main.py", "x", nil)
	if err == nil {
		t.Fatal("a 409 came back clean")
	}
	if StatusOf(err) != StatusConflict || StatusOf(err) == 400 {
		t.Errorf("status = %d, want %d", StatusOf(err), StatusConflict)
	}
	// The server's prose is the only detail a 409 carries — there is no
	// structured payload — so it has to survive into the error.
	if !strings.Contains(err.Error(), "changed since you last read it") {
		t.Errorf("error = %v, want the server's own message", err)
	}
}
