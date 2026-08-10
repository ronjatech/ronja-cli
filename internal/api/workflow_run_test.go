package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// A realistic GET /workflow/run/:runID body, with the run row at the ROOT
// alongside steps and health — the flattening the server gets from embedding
// *rdb.WorkflowRun in RunResponse.
//
// Same purpose as the workflow fixture: tag verification. It matters more here,
// because `wf test --json` prints this struct verbatim, so a mistyped tag does
// not merely go unread — it silently deletes a field from the CLI's output.
const runResponseFixture = `{
  "tenant_id": "ten-1",
  "id": "run-abc",
  "workflowID": "wf-1",
  "parameterValues": {"month": "2026-07", "limit": 12},
  "status": "done",
  "outputs": [{"fileKey": "runs/x.html", "format": "html", "fileID": "file-1", "name": "report.html"}],
  "tableOutputs": [{"modelID": "tbl-1", "logicalName": "revenue", "displayName": "Monthly revenue",
                    "writeMode": "replace", "fileCount": 3}],
  "logs": "line one\nline two\n",
  "error": null,
  "executedBy": "usr-1",
  "executedAt": "2026-07-28T14:00:00Z",
  "completedAt": "2026-07-28T14:00:12Z",
  "execRunID": "exec-1",
  "traceID": "trace-1",
  "triggeringRunKind": null,
  "triggeringRunID": null,
  "processingStartedAt": "2026-07-28T14:00:03Z",
  "kind": "report",
  "body": null,
  "tokensUsed": 120,
  "aiCallCount": 2,
  "queryCount": 5,
  "steps": [
    {"id": "span-1", "parentStepId": null, "seq": 0, "name": "Fetch data", "status": "done",
     "startedAt": "2026-07-28T14:00:03Z", "endedAt": "2026-07-28T14:00:07Z", "durationMs": 4000,
     "aiCallCount": 0, "queryCount": 3, "tokensUsed": 0, "error": null, "logs": "fetched"}
  ],
  "health": "done"
}`

func TestGetWorkflowRunDecodesTheFlattenedResponse(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/workflow/run/run-abc" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(runResponseFixture))
	})

	run, err := client.GetWorkflowRun(context.Background(), "run-abc")
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}

	// The run row, reached through the embedded struct exactly as the server
	// serves it: at the root, not under a "run" key.
	if run.ID != "run-abc" || run.WorkflowID != "wf-1" || run.Status != RunStatusDone {
		t.Errorf("run identity = %+v", run.WorkflowRun)
	}
	if run.Running() {
		t.Error("a done run reports itself as still running")
	}
	if run.Health != RunHealthDone {
		t.Errorf("health = %q", run.Health)
	}
	if run.Logs != "line one\nline two\n" {
		t.Errorf("logs = %q", run.Logs)
	}
	if run.Error != nil {
		t.Errorf("error = %v, want nil for a null", *run.Error)
	}
	if run.CompletedAt == nil || run.ExecutedAt.IsZero() {
		t.Errorf("timestamps = %v / %v", run.CompletedAt, run.ExecutedAt)
	}
	if run.ProcessingStartedAt == nil {
		t.Error("processingStartedAt did not decode")
	}
	if run.ExecRunID == nil || *run.ExecRunID != "exec-1" || run.TraceID == nil {
		t.Errorf("telemetry links = %v / %v", run.ExecRunID, run.TraceID)
	}
	if run.TriggeringRunID != nil {
		t.Error("a null triggeringRunID decoded to a value")
	}
	if run.Kind != "report" || run.TokensUsed != 120 || run.AICallCount != 2 || run.QueryCount != 5 {
		t.Errorf("counters = %+v", run.WorkflowRun)
	}
	if len(run.ParameterValues) != 2 {
		t.Errorf("parameterValues = %+v", run.ParameterValues)
	}

	if len(run.Outputs) != 1 || run.Outputs[0].Name != "report.html" || run.Outputs[0].Format != "html" {
		t.Errorf("outputs = %+v", run.Outputs)
	}
	if len(run.TableOutputs) != 1 {
		t.Fatalf("tableOutputs = %+v", run.TableOutputs)
	}
	table := run.TableOutputs[0]
	if table.ModelID != "tbl-1" || table.DisplayName != "Monthly revenue" || table.WriteMode != "replace" || table.FileCount != 3 {
		t.Errorf("tableOutput = %+v", table)
	}

	if len(run.Steps) != 1 {
		t.Fatalf("steps = %+v", run.Steps)
	}
	step := run.Steps[0]
	if step.ID != "span-1" || step.Name != "Fetch data" || step.Status != "done" {
		t.Errorf("step = %+v", step)
	}
	if step.DurationMs == nil || *step.DurationMs != 4000 {
		t.Errorf("durationMs = %v", step.DurationMs)
	}
	if step.EndedAt == nil || step.Logs == nil || *step.Logs != "fetched" {
		t.Errorf("step optionals = %v / %v", step.EndedAt, step.Logs)
	}
	if step.ParentStepID != nil || step.Error != nil {
		t.Error("a null parentStepId or error decoded to a value")
	}
	if step.QueryCount != 3 {
		t.Errorf("step queryCount = %d", step.QueryCount)
	}
}

// An unrecognised status must END the poll rather than loop until --timeout: a
// status added server-side is not a reason for the CLI to hang.
func TestRunningIsTrueOnlyWhileRunning(t *testing.T) {
	for status, want := range map[string]bool{
		RunStatusRunning: true,
		RunStatusDone:    false,
		RunStatusError:   false,
		"cancelled":      false,
		"":               false,
	} {
		run := &RunResponse{WorkflowRun: WorkflowRun{Status: status}}
		if got := run.Running(); got != want {
			t.Errorf("Running() for %q = %v, want %v", status, got, want)
		}
	}
}

// A run of a workflow with no parameters must send parameterValues as {}, NOT
// omit it. The server decodes a missing key to a nil map exactly as it does an
// explicit null, and parameter_values is NOT NULL — omitting the field is the
// null map this is meant to avoid, and used to fail the run with an opaque 400.
func TestRunWorkflowSendsEmptyParameterValues(t *testing.T) {
	var bodies []map[string]any
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{"id":"run-1","workflowID":"wf-1","status":"running"}`))
	})

	if _, err := client.RunWorkflow(context.Background(), "wf-1", nil); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}
	if _, err := client.RunWorkflow(context.Background(), "wf-1", map[string]any{"month": "2026-07"}); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	empty, ok := bodies[0]["parameterValues"].(map[string]any)
	if !ok {
		t.Errorf("empty parameters omitted the key (server reads that as null): %+v", bodies[0])
	} else if len(empty) != 0 {
		t.Errorf("empty parameters sent %+v, want {}", empty)
	}
	values, ok := bodies[1]["parameterValues"].(map[string]any)
	if !ok || values["month"] != "2026-07" {
		t.Errorf("parameterValues = %+v", bodies[1])
	}
}

// The name lookup is best effort, and an unnamed table is a real state.
func TestGetTableDecodesAnAbsentName(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/feature/model/tbl-1" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"tbl-1","name":null,"description":"x"}`))
	})

	table, err := client.GetTable(context.Background(), "tbl-1")
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	if table.ID != "tbl-1" || table.Name != nil {
		t.Errorf("table = %+v", table)
	}
}
