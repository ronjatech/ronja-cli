package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// metricRowFixture is a metric GET response in feature.TableView's full shape,
// including fields this mirror does not carry. The recipe's keys are out of
// alphabetical order on purpose: the compare-and-swap hashes the bytes received,
// and sorted keys would let a client that re-marshalled pass.
const metricRowFixture = `{
  "id": "table-aov",
  "featureID": "collection-1",
  "name": "Average order value",
  "description": "Revenue divided by orders, per day",
  "descriptionSource": "user",
  "kind": "metric",
  "status": "ready",
  "code": "SELECT ...",
  "engine": "duckdb",
  "inputModels": ["table-orders"],
  "parquetKey": null,
  "fields": [],
  "hidden": false,
  "archived": false,
  "parentModelID": "",
  "shadowStatus": "",
  "reportingTimezone": "Europe/Stockholm",
  "metricRecipe": {"value":"revenue / orders","source":"table-orders","version":1},
  "metricStatus": "verified",
  "metricIsAdditive": false,
  "metricTimeColumn": "created_at",
  "metricValueColumn": null,
  "metricDimensions": ["country"],
  "ownerID": "user-1",
  "verifiedBy": "user-9",
  "verifiedAt": "2026-08-01T09:00:00Z",
  "retiredAt": null,
  "flattenedSQL": "SELECT ...",
  "definitionHash": "abc123",
  "verifiedAgainstDefinitionHash": "abc123",
  "createdAt": "2026-07-01T08:00:00Z",
  "updatedAt": "2026-07-28T10:30:00Z",
  "createdBy": "user-1",
  "updatedBy": "user-1",
  "url": "https://app.example.test/tables/table-aov",
  "buildVerdict": "ok"
}`

func TestGetTableDecodesTheMetricAxis(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeString(w, metricRowFixture)
	})
	row, err := client.GetTable(context.Background(), "table-aov")
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	if row.Kind != TableKindMetric {
		t.Errorf("kind = %q", row.Kind)
	}
	if row.MetricStatus != MetricStatusVerified {
		t.Errorf("metricStatus = %q", row.MetricStatus)
	}
	// Verbatim, key order included: the compare-and-swap digest is taken over
	// these bytes.
	const want = `{"value":"revenue / orders","source":"table-orders","version":1}`
	if string(row.MetricRecipe) != want {
		t.Errorf("metricRecipe = %s\nwant %s", row.MetricRecipe, want)
	}
}

// A non-metric row answers `"metricRecipe": null`, which decodes into a
// four-byte RawMessage that is neither empty nor valid to send.
func TestNormalizeFoldsANullRecipe(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeString(w, `{"id":"table-orders","kind":"derived","metricRecipe":null,"metricStatus":null,"inputModels":null}`)
	})
	row, err := client.GetTable(context.Background(), "table-orders")
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	if row.MetricRecipe != nil {
		t.Errorf("metricRecipe = %q, want nil", row.MetricRecipe)
	}
	if row.MetricStatus != "" {
		t.Errorf("metricStatus = %q", row.MetricStatus)
	}
	if row.Kind == TableKindMetric {
		t.Error("a derived row must not read as a metric")
	}
}

// A create body carrying a verification field or derived metric column would
// let a caller mint a "verified" metric attributed to someone else; empty fields
// are omitted so the body never appears to claim them.
func TestCreateMetricSendsOnlyWhatTheBodyPublishes(t *testing.T) {
	var sent map[string]any
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/feature/model/metric" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeString(w, `{"metric":`+metricRowFixture+`,"warnings":["the source closure could not be resolved"]}`)
	})

	out, err := client.CreateMetric(context.Background(), CreateMetricInput{
		Name:      "Average order value",
		FeatureID: "collection-1",
		Recipe:    json.RawMessage(`{"source":"table-orders","value":"revenue / orders"}`),
	})
	if err != nil {
		t.Fatalf("CreateMetric: %v", err)
	}
	for _, forbidden := range []string{
		"metricStatus", "verifiedAt", "verifiedBy", "verifiedAgainstDefinitionHash",
		"metricIsAdditive", "metricTimeColumn", "metricValueColumn", "metricDimensions",
		"metricSourceClosure", "code", "kind",
	} {
		if _, present := sent[forbidden]; present {
			t.Errorf("the create body published %q", forbidden)
		}
	}
	for _, omitted := range []string{"description", "reportingTimezone"} {
		if _, present := sent[omitted]; present {
			t.Errorf("an empty %q was sent instead of being omitted", omitted)
		}
	}
	if out.Metric == nil || out.Metric.ID != "table-aov" {
		t.Fatalf("metric = %+v", out.Metric)
	}
	if !reflect.DeepEqual(out.Warnings, []string{"the source closure could not be resolved"}) {
		t.Errorf("warnings = %v", out.Warnings)
	}
}

// The server 400s an empty baseRecipeSha256 rather than reading it as "no
// check". reportingTimezone is the same trap reversed: absent leaves the
// declaration alone, an explicit empty resets it to UTC.
func TestWriteMetricRecipeOmitsAnAbsentPrecondition(t *testing.T) {
	var sent map[string]any
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v2/feature/model/table-draft/metric/recipe" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeString(w, `{"draftID":"table-draft","metricID":"table-aov","recipe":{"source":"table-orders"},"additive":false,"dimensions":null,"timeColumn":"created_at","inputModels":null,"sourceClosure":null,"reportingTimezone":""}`)
	})

	out, err := client.WriteMetricRecipe(context.Background(), "table-draft", WriteMetricRecipeInput{
		Recipe: json.RawMessage(`{"source":"table-orders"}`),
	})
	if err != nil {
		t.Fatalf("WriteMetricRecipe: %v", err)
	}
	if _, present := sent["baseRecipeSha256"]; present {
		t.Error("an unconditional write sent baseRecipeSha256, which the server answers 400")
	}
	if _, present := sent["reportingTimezone"]; present {
		t.Error("an absent reportingTimezone was sent, which would reset the metric's calendar")
	}

	// Only sourceClosure is normalised server-side; the other two can arrive as
	// JSON null on a healthy write.
	if out.Dimensions == nil || out.InputModels == nil || out.SourceClosure == nil {
		t.Errorf("Normalize left a nil slice: %+v", out)
	}
	if out.MetricID != "table-aov" || out.TimeColumn != "created_at" {
		t.Errorf("result = %+v", out)
	}
}

func TestWriteMetricRecipeSendsAnExplicitTimezoneReset(t *testing.T) {
	var sent map[string]any
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeString(w, `{"draftID":"table-draft","metricID":"table-aov","recipe":{},"dimensions":[],"inputModels":[],"sourceClosure":[]}`)
	})
	empty := ""
	if _, err := client.WriteMetricRecipe(context.Background(), "table-draft", WriteMetricRecipeInput{
		ReportingTimezone: &empty,
		BaseRecipeSha256:  strings.Repeat("a", 64),
	}); err != nil {
		t.Fatalf("WriteMetricRecipe: %v", err)
	}
	if got, present := sent["reportingTimezone"]; !present || got != "" {
		t.Errorf("reportingTimezone = %v (present=%v), want an explicit empty string", got, present)
	}
	if got := sent["baseRecipeSha256"]; got != strings.Repeat("a", 64) {
		t.Errorf("baseRecipeSha256 = %v", got)
	}
	if _, present := sent["recipe"]; present {
		t.Error("an absent recipe was sent, which would replace the draft's definition with nothing")
	}
}

func writeString(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		panic(err)
	}
}
