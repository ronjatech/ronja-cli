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

// metricRowFixture is a realistic single-row GET response for a METRIC, in the
// shape feature.TableView actually serves — including the fields this mirror
// deliberately does not carry, so the test proves the tags MATCH rather than
// that a stripped-down fixture happens to decode.
//
// The recipe's keys are spelled OUT of alphabetical order on purpose. The whole
// compare-and-swap depends on the client hashing the bytes it received rather
// than a re-marshal of them, and a fixture whose keys were already sorted would
// let a client that re-marshalled pass this test and be refused in production.
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

// TestGetTableDecodesTheMetricAxis: the two metric fields this mirror carries,
// and the exact bytes of the one that matters.
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
	// VERBATIM, including the key order. This is the byte string the compare-and-
	// swap digest is taken over, and a decoder that normalised it would break the
	// precondition against an edit nobody made.
	const want = `{"value":"revenue / orders","source":"table-orders","version":1}`
	if string(row.MetricRecipe) != want {
		t.Errorf("metricRecipe = %s\nwant %s", row.MetricRecipe, want)
	}
}

// TestNormalizeFoldsANullRecipe: `metricRecipe` carries no omitempty
// server-side, so EVERY row answers with the key and a non-metric one answers
// `null` — which decodes into a four-byte RawMessage that is neither empty nor
// valid to send anywhere. One spelling of "no recipe" keeps the fingerprint
// helpers from having to know two.
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
	// A null optional decodes into a plain string as a no-op, which is exactly
	// what "unset" means for this field.
	if row.MetricStatus != "" {
		t.Errorf("metricStatus = %q", row.MetricStatus)
	}
	if row.Kind == TableKindMetric {
		t.Error("a derived row must not read as a metric")
	}
}

// TestCreateMetricSendsOnlyWhatTheBodyPublishes.
//
// The two assertions are about the same rule from both sides. What is SENT must
// not carry a verification field or a derived metric column — a body that could
// would let a caller mint a row asserting it is a verified metric attributed to
// someone else, which is the hole the server's create body was narrowed to
// close. And a field left empty must be OMITTED rather than sent as "" — not
// because the two mean different things to the server (they do not: the handler
// collapses an absent description onto "" and stamps prose only when the
// trimmed value is non-empty), but because a body that publishes a field it is
// not claiming invites a reader to think it is claiming one. See
// CreateMetricInput.Description.
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

// TestWriteMetricRecipeOmitsAnAbsentPrecondition is the tag that is load-bearing
// in the OTHER direction, and it is the one that 400s every ordinary push if it
// is dropped: the server reads an empty baseRecipeSha256 as a client bug rather
// than as "no check", deliberately, so a caller who thought they were asserting
// something is told they were not.
//
// The reporting timezone is asserted alongside it because it is the same trap
// with the opposite polarity: absent LEAVES the declaration alone, an explicit
// empty RESETS it, so a pointer that collapsed to "" would silently move every
// metric's calendar to UTC on every push.
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

	// THE THREE NIL SLICES. Only sourceClosure is normalised server-side, so the
	// other two can arrive as JSON null on a perfectly healthy write — and a nil
	// that reached a report as "no dimensions" is indistinguishable from a metric
	// that really declares none.
	if out.Dimensions == nil || out.InputModels == nil || out.SourceClosure == nil {
		t.Errorf("Normalize left a nil slice: %+v", out)
	}
	if out.MetricID != "table-aov" || out.TimeColumn != "created_at" {
		t.Errorf("result = %+v", out)
	}
}

// TestWriteMetricRecipeSendsAnExplicitTimezoneReset: the third state, which only
// a pointer can express — send an explicit empty string and the server resets the
// declaration.
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

// writeString answers a canned body, so each fixture above reads as the response
// a real instance sends rather than as a builder.
func writeString(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		panic(err)
	}
}
