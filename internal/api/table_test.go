package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tableFixture is in feature.TableView's full shape, including fields this
// mirror does not carry. A tag that drifts from tableview.go decodes silently to
// zero: an empty `code` would push a colleague's table to nothing, an absent
// buildVerdict would report a failed build as success.
const tableFixture = `{
  "id": "table-abc",
  "featureID": "collection-1",
  "sessionID": null,
  "name": "monthly_revenue",
  "description": "Revenue rolled up by month",
  "descriptionSource": "ai",
  "kind": "derived",
  "status": "ready",
  "modelConnectorID": null,
  "code": "SELECT * FROM {{ ref('0') }}",
  "engine": "duckdb",
  "inputModels": ["table-orders"],
  "logicSummary": [],
  "computeRoute": null,
  "processingStrategy": null,
  "partitionKey": null,
  "bigData": false,
  "tableRole": null,
  "parquetKey": "tables/derived/table-abc/data.parquet",
  "previewParquetKey": null,
  "csvFileKey": null,
  "partitioned": true,
  "buildingSince": null,
  "lastBuiltTimezone": "Europe/Stockholm",
  "reportingTimezone": "Europe/Stockholm",
  "fields": [],
  "hidden": false,
  "archived": false,
  "parentModelID": "table-parent",
  "shadowStatus": "draft",
  "committedAt": null,
  "drafterUserID": "user-1",
  "drafterKind": "user",
  "drafterSessionID": null,
  "submittedForReviewAt": null,
  "workflowID": null,
  "workflowLogicalName": "",
  "markedForDeletionAt": null,
  "markedForDeletionBy": null,
  "pendingMoveRequestID": null,
  "metricRecipe": null,
  "metricStatus": null,
  "metricDimensions": [],
  "createdAt": "2026-07-01T08:00:00Z",
  "updatedAt": "2026-07-28T10:30:00Z",
  "createdBy": "user-1",
  "updatedBy": "user-1",
  "url": "https://app.example.test/tables/table-abc",
  "lastBuildError": {
    "message": "the transformation is impossible with the given inputs",
    "type": "invalid_input",
    "occurredAt": "2026-07-28T10:29:00Z"
  },
  "buildVerdict": "failed_stale",
  "inputRefs": [{"id": "table-orders", "name": "orders", "accessible": true}]
}`

func TestGetTableDecodesEveryMirroredField(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/feature/model/table-abc" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(tableFixture))
	})

	tbl, err := client.GetTable(context.Background(), "table-abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for _, c := range []struct {
		field string
		got   any
		want  any
	}{
		{"ID", tbl.ID, "table-abc"},
		{"FeatureID", tbl.FeatureID, "collection-1"},
		{"Name", tbl.Name, "monthly_revenue"},
		{"Kind", tbl.Kind, TableKindDerived},
		{"Status", tbl.Status, TableStatusReady},
		{"Code", tbl.Code, "SELECT * FROM {{ ref('0') }}"},
		{"ParentModelID", tbl.ParentModelID, "table-parent"},
		{"ShadowStatus", tbl.ShadowStatus, ShadowStatusDraft},
		{"BuildVerdict", tbl.BuildVerdict, BuildVerdictFailedStale},
		{"URL", tbl.URL, "https://app.example.test/tables/table-abc"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if !reflect.DeepEqual(tbl.InputModels, []string{"table-orders"}) {
		t.Errorf("InputModels = %v", tbl.InputModels)
	}
	if tbl.LastBuildError == nil {
		t.Fatal("LastBuildError decoded to nil — the whole reason a failed push says anything useful")
	}
	if tbl.LastBuildError.Message != "the transformation is impossible with the given inputs" ||
		tbl.LastBuildError.Type != "invalid_input" ||
		!tbl.LastBuildError.OccurredAt.Equal(time.Date(2026, 7, 28, 10, 29, 0, 0, time.UTC)) {
		t.Errorf("LastBuildError = %+v", tbl.LastBuildError)
	}
	if !tbl.UpdatedAt.Equal(time.Date(2026, 7, 28, 10, 30, 0, 0, time.UTC)) {
		t.Errorf("UpdatedAt = %v", tbl.UpdatedAt)
	}

	// The stored form is positional; everything downstream hashes and writes the
	// canonical form.
	code, unresolved := tbl.CanonicalCode()
	if unresolved || code != "SELECT * FROM {{ ref('table-orders') }}" {
		t.Errorf("CanonicalCode = %q, unresolved=%v", code, unresolved)
	}
	if tbl.ShadowStatus != ShadowStatusDraft || tbl.ParentModelID != "table-parent" {
		t.Errorf("shadow markers = %q / %q", tbl.ShadowStatus, tbl.ParentModelID)
	}
	if tbl.IdentityID() != "table-parent" {
		t.Errorf("IdentityID = %q — a draft's identity is its parent", tbl.IdentityID())
	}
}

// An older instance can send null, which must not read as a lineage difference.
func TestGetTableNormalizesAbsentInputModels(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"table-1","inputModels":null}`))
	})
	tbl, err := client.GetTable(context.Background(), "table-1")
	if err != nil {
		t.Fatal(err)
	}
	if tbl.InputModels == nil || len(tbl.InputModels) != 0 {
		t.Errorf("InputModels = %v", tbl.InputModels)
	}
}

// The endpoint answers 200 with a JSON `null` for no draft, which must not
// decode into a zero-value Table.
func TestGetTableDraftAnswersNilForNoDraft(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/feature/model/table-abc/draft" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`null`))
	})
	draft, err := client.GetTableDraft(context.Background(), "table-abc")
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft != nil {
		t.Errorf("no draft must decode to nil, got %+v", draft)
	}
}

// A 404 on /draft is the PARENT table's read gate, never "no draft".
func TestGetTableDraftNamesTheUnreachableParent(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	draft, err := client.GetTableDraft(context.Background(), "table-abc")
	if err == nil {
		t.Fatalf("a 404 must be an error, got draft %+v", draft)
	}
	if StatusOf(err) != http.StatusNotFound {
		t.Errorf("StatusOf = %d, want 404 (the *Error must stay wrapped)", StatusOf(err))
	}
	if msg := err.Error(); !strings.Contains(msg, "table table-abc is not reachable") || !strings.Contains(msg, "trash") {
		t.Errorf("error = %q, want it to name the unreachable table and the trash", msg)
	}
}

func TestGetTableDraftDecodesADraft(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tableFixture))
	})
	draft, err := client.GetTableDraft(context.Background(), "table-parent")
	if err != nil {
		t.Fatal(err)
	}
	if draft == nil || draft.ID != "table-abc" || draft.ShadowStatus != ShadowStatusDraft {
		t.Errorf("draft = %+v", draft)
	}
}

// reviewFixture mirrors governance.TableDraftReview verbatim.
const reviewFixture = `{
  "draftID": "table-draft-1",
  "tableID": "table-abc",
  "draftSQL": "SELECT a, b FROM {{ ref('table-orders') }}",
  "liveSQL": "SELECT a FROM {{ ref('table-orders') }}",
  "fieldsDelta": [
    {"name": "b", "change": "added"},
    {"name": "c", "change": "removed"},
    {"name": "d", "change": "retyped", "draftType": "BIGINT", "liveType": "VARCHAR"}
  ],
  "inputModelsDelta": {"added": ["table-refunds"], "removed": ["table-legacy"]},
  "draftRowCount": 4210,
  "liveRowCount": -1,
  "baseStale": true,
  "interveningVersions": [
    {"versionID": "table-v2", "name": "monthly_revenue", "committedAt": "2026-07-27T12:00:00Z"}
  ],
  "headVersionID": "table-v2",
  "submittedForReview": true,
  "drafterUserID": "user-1"
}`

func TestGetTableDraftReviewDecodesTheWholePayload(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// The route is on the GOVERNANCE handler, not under /feature/model.
		if r.URL.Path != "/api/v2/table/draft/table-draft-1/review" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(reviewFixture))
	})

	rev, err := client.GetTableDraftReview(context.Background(), "table-draft-1")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if rev.DraftID != "table-draft-1" || rev.TableID != "table-abc" {
		t.Errorf("ids = %q / %q", rev.DraftID, rev.TableID)
	}
	want := []TableFieldDelta{
		{Name: "b", Change: "added"},
		{Name: "c", Change: "removed"},
		{Name: "d", Change: "retyped", DraftType: "BIGINT", LiveType: "VARCHAR"},
	}
	if !reflect.DeepEqual(rev.FieldsDelta, want) {
		t.Errorf("fieldsDelta = %+v", rev.FieldsDelta)
	}
	// The one change a commit can make with no SQL diff at all.
	if rev.InputModelsDelta == nil {
		t.Fatal("inputModelsDelta did not decode")
	}
	if !reflect.DeepEqual(rev.InputModelsDelta, &TableInputModelsDelta{
		Added: []string{"table-refunds"}, Removed: []string{"table-legacy"},
	}) {
		t.Errorf("inputModelsDelta = %+v", rev.InputModelsDelta)
	}
	if rev.DraftRowCount != 4210 {
		t.Errorf("draftRowCount = %d", rev.DraftRowCount)
	}
	// -1 means "never built", not an empty table.
	if rev.LiveRowCount != -1 {
		t.Errorf("liveRowCount = %d, want the -1 sentinel", rev.LiveRowCount)
	}
	if !rev.BaseStale || len(rev.InterveningVersions) != 1 ||
		rev.InterveningVersions[0].VersionID != "table-v2" ||
		!rev.InterveningVersions[0].CommittedAt.Equal(time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("staleness = %v / %+v", rev.BaseStale, rev.InterveningVersions)
	}
	if !rev.SubmittedForReview {
		t.Error("submittedForReview did not decode")
	}
	// `pipeline publish --overwrite-remote` sends this back as
	// confirmHeadVersionID; empty, the override has nothing to confirm.
	if rev.HeadVersionID != "table-v2" {
		t.Errorf("headVersionID = %q, want the id the override confirms", rev.HeadVersionID)
	}
}

// The server 400s an explicit empty baseCodeSha256
// (rmodelv2.checkCodePrecondition), so without omitempty every unconditional
// write would be refused.
func TestUpdateTableCodeSendsBaseCodeSha256OnlyWhenAsserting(t *testing.T) {
	const digest = "b5bb9d8014a0f9b1d61e21e796d78dccdf1352f23cd32812f4850b878ae4944c"
	for _, tt := range []struct {
		name    string
		basis   string
		present bool
	}{
		{"an unconditional write sends no precondition at all", "", false},
		{"a digest is sent verbatim", digest, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []capture
			client := serveCapturing(t, 200, "", &got)
			if err := client.UpdateTableCode(context.Background(), "table-draft-1", "SELECT 1", []string{}, tt.basis); err != nil {
				t.Fatalf("update: %v", err)
			}
			raw, present := got[0].body["baseCodeSha256"]
			if present != tt.present {
				t.Fatalf("baseCodeSha256 present = %v, want %v (body %+v)", present, tt.present, got[0].body)
			}
			if tt.present && raw != tt.basis {
				t.Errorf("baseCodeSha256 = %v, want %q", raw, tt.basis)
			}
		})
	}
}

// Only the override puts a body on a commit, and its field name must match
// rmodelv2.CommitDraftInput: a misspelt tag reads server-side as "no override".
func TestCommitTableDraftSendsNoBodyWithoutAnOverride(t *testing.T) {
	var got []capture
	client := serveCapturing(t, 200, "", &got)
	if err := client.CommitTableDraft(context.Background(), "table-draft-1", ""); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(got[0].body) != 0 {
		t.Errorf("body = %+v, want nothing sent", got[0].body)
	}
	got = nil
	client = serveCapturing(t, 200, "", &got)
	if err := client.CommitTableDraft(context.Background(), "table-draft-1", "table-v2"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got[0].body["confirmHeadVersionID"] != "table-v2" {
		t.Errorf("body = %+v", got[0].body)
	}
}

// The token is an OFFSET present only while more rows remain; a full first page
// is not the end.
func TestListFeatureTablesPaginates(t *testing.T) {
	var paths []string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Query().Get("featureID") != "collection-1" {
			t.Errorf("featureID = %q", r.URL.Query().Get("featureID"))
		}
		if r.URL.Query().Get("token") == "" {
			_, _ = w.Write([]byte(`{"token":"1","total":2,"result":[
				{"id":"table-a","name":"a","kind":"derived","status":"ready","hasError":false,
				 "updatedAt":"2026-07-01T00:00:00Z","featureID":"collection-1"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":null,"total":2,"result":[
			{"id":"table-b","name":"b","kind":"dynamic","status":"build_failed","hasError":true,
			 "updatedAt":"2026-07-02T00:00:00Z","featureID":"collection-1"}]}`))
	})

	rows, err := client.ListFeatureTables(context.Background(), "collection-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "table-a" || rows[1].ID != "table-b" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Kind != TableKindDerived || rows[0].HasError {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].Status != TableStatusBuildFailed || !rows[1].HasError {
		t.Errorf("row 1 = %+v", rows[1])
	}
	if !rows[1].UpdatedAt.Equal(time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("updatedAt = %v", rows[1].UpdatedAt)
	}
	if len(paths) != 2 {
		t.Fatalf("expected two requests, got %v", paths)
	}
}

// An instance answering the same offset again would loop forever; the guard is
// not a cap on how many tables a feature may have.
func TestListFeatureTablesStopsOnANonAdvancingToken(t *testing.T) {
	var calls int32
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"token":"1","total":99,"result":[{"id":"table-a"}]}`))
	})
	rows, err := client.ListFeatureTables(context.Background(), "collection-1")
	if err != nil {
		t.Fatalf("a stuck token must not be an error, it must just end: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected the loop to stop on the repeated token, made %d requests", got)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %+v", rows)
	}
}

// ── Writes ──────────────────────────────────────────────────────────────────

type capture struct {
	method string
	path   string
	body   map[string]any
}

func serveCapturing(t *testing.T, status int, response string, into *[]capture) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := capture{method: r.Method, path: r.URL.Path}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &c.body)
		}
		*into = append(*into, c)
		w.WriteHeader(status)
		if response != "" {
			_, _ = w.Write([]byte(response))
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-token")
}

func TestCreateTableSendsTheDerivedShape(t *testing.T) {
	var got []capture
	client := serveCapturing(t, 200, tableFixture, &got)

	out, err := client.CreateTable(context.Background(), CreateTableInput{
		Name:        "monthly_revenue",
		FeatureID:   "collection-1",
		Kind:        TableKindDerived,
		Engine:      TableEngineDuckDB,
		Code:        "SELECT * FROM {{ ref('table-orders') }}",
		InputModels: []string{"table-orders"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.ID != "table-abc" {
		t.Errorf("id = %q", out.ID)
	}
	if len(got) != 1 || got[0].method != "POST" || got[0].path != "/api/v2/feature/model" {
		t.Fatalf("request = %+v", got)
	}
	body := got[0].body
	for _, c := range []struct{ key, want string }{
		{"name", "monthly_revenue"},
		{"featureID", "collection-1"},
		{"kind", "derived"},
		{"engine", "duckdb"},
		{"code", "SELECT * FROM {{ ref('table-orders') }}"},
	} {
		if body[c.key] != c.want {
			t.Errorf("body[%q] = %v, want %q", c.key, body[c.key], c.want)
		}
	}
	if !reflect.DeepEqual(body["inputModels"], []any{"table-orders"}) {
		t.Errorf("body[inputModels] = %v", body["inputModels"])
	}
}

// Server-side derivation fires only when the declared list is EMPTY and a
// draft inherits its parent's input_models, so an omitted field 400s once an
// edit adds a ref; an explicit empty list is a real declaration omitempty must
// not drop.
func TestUpdateTableCodeAlwaysSendsInputModels(t *testing.T) {
	for _, tt := range []struct {
		name   string
		inputs []string
		want   []any
	}{
		{"a declared set", []string{"table-orders"}, []any{"table-orders"}},
		{"an explicit empty declaration", []string{}, []any{}},
		{"a nil is sent as an empty declaration, never omitted", nil, []any{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []capture
			client := serveCapturing(t, 200, "", &got)
			if err := client.UpdateTableCode(context.Background(), "table-draft-1", "SELECT 1", tt.inputs, ""); err != nil {
				t.Fatalf("update: %v", err)
			}
			if len(got) != 1 || got[0].method != "PUT" || got[0].path != "/api/v2/feature/model/table-draft-1" {
				t.Fatalf("request = %+v", got)
			}
			raw, present := got[0].body["inputModels"]
			if !present {
				t.Fatal("inputModels was omitted — server-side derivation would re-arm and the next added ref would 400")
			}
			if !reflect.DeepEqual(raw, tt.want) {
				t.Errorf("inputModels = %v, want %v", raw, tt.want)
			}
			if got[0].body["code"] != "SELECT 1" {
				t.Errorf("code = %v", got[0].body["code"])
			}
		})
	}
}

// request-review is on a different prefix (the governance handler) from the
// other lifecycle verbs.
func TestLifecycleVerbsHitTheRightRoutes(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(*Client) error
		want string
	}{
		{"checkout", func(c *Client) error {
			_, err := c.CheckoutTable(context.Background(), "table-abc")
			return err
		}, "/api/v2/feature/model/table-abc/checkout"},
		{"sync", func(c *Client) error {
			return c.SyncTable(context.Background(), "table-draft-1")
		}, "/api/v2/feature/model/table-draft-1/sync"},
		{"commit", func(c *Client) error {
			return c.CommitTableDraft(context.Background(), "table-draft-1", "")
		}, "/api/v2/feature/model/table-draft-1/commit"},
		{"discard", func(c *Client) error {
			return c.DiscardTableDraft(context.Background(), "table-draft-1")
		}, "/api/v2/feature/model/table-draft-1/discard"},
		{"request-review is on the governance handler", func(c *Client) error {
			return c.RequestTableReview(context.Background(), "table-draft-1")
		}, "/api/v2/table/draft/table-draft-1/request-review"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []capture
			client := serveCapturing(t, 200, tableFixture, &got)
			if err := tt.call(client); err != nil {
				t.Fatalf("call: %v", err)
			}
			if len(got) != 1 || got[0].method != "POST" || got[0].path != tt.want {
				t.Errorf("request = %+v, want POST %s", got, tt.want)
			}
		})
	}
}

// ── The poll loop ───────────────────────────────────────────────────────────

func fastPoll(c *Client) *Client {
	c.TableBuild = TableBuildPoll{
		Interval:    time.Millisecond,
		MaxInterval: 2 * time.Millisecond,
		Timeout:     2 * time.Second,
		// The failure tolerance is elapsed time, not a retry count.
		MaxFailureWindow: 20 * time.Millisecond,
		RateLimitBackoff: time.Millisecond,
	}
	return c
}

func tableStates(t *testing.T, bodies ...string) *Client {
	t.Helper()
	var i int32
	return fastPoll(serve(t, func(w http.ResponseWriter, _ *http.Request) {
		n := int(atomic.AddInt32(&i, 1)) - 1
		if n >= len(bodies) {
			n = len(bodies) - 1
		}
		_, _ = w.Write([]byte(bodies[n]))
	}))
}

func TestWaitForTableBuildPollsToATerminalVerdict(t *testing.T) {
	client := tableStates(t,
		`{"id":"table-1","status":"pending","buildVerdict":"pending"}`,
		`{"id":"table-1","status":"building","buildVerdict":"building"}`,
		`{"id":"table-1","status":"ready","buildVerdict":"ok"}`,
	)
	res, err := client.WaitForTableBuild(context.Background(), "table-1")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Verdict != BuildVerdictOK || !res.OK() || res.Derived {
		t.Errorf("result = %+v", res)
	}
}

func TestWaitForTableBuildReturnsAFailureAsData(t *testing.T) {
	client := tableStates(t, `{"id":"table-1","status":"ready","buildVerdict":"failed_stale",
		"lastBuildError":{"message":"impossible with the given inputs","type":"invalid_input"}}`)
	res, err := client.WaitForTableBuild(context.Background(), "table-1")
	if err != nil {
		t.Fatalf("a failed build must not be an error: %v", err)
	}
	if res.Verdict != BuildVerdictFailedStale || res.OK() {
		t.Errorf("result = %+v", res)
	}
	if res.ErrorMessage() != "impossible with the given inputs" {
		t.Errorf("ErrorMessage = %q", res.ErrorMessage())
	}
}

func TestWaitForTableBuildTreatsOKPartialAsUsable(t *testing.T) {
	client := tableStates(t, `{"id":"table-1","status":"ready","buildVerdict":"ok_partial"}`)
	res, err := client.WaitForTableBuild(context.Background(), "table-1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Errorf("ok_partial is usable, got %+v", res)
	}
}

// For an instance with no buildVerdict, `ready` with an error is a FAILED build
// still serving the previous build's data, not a success.
func TestWaitForTableBuildFallsBackOnAnOlderServer(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"ready, clean", `{"id":"t","status":"ready"}`, BuildVerdictOK},
		{"ready, partial-data notice", `{"id":"t","status":"ready","lastBuildError":{"message":"skipped files","type":"partial_data_corrupt"}}`, BuildVerdictOKPartial},
		{"ready, real failure", `{"id":"t","status":"ready","lastBuildError":{"message":"boom","type":"invalid_input"}}`, BuildVerdictFailedStale},
		{"build_failed", `{"id":"t","status":"build_failed"}`, BuildVerdictFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := tableStates(t, tt.body)
			res, err := client.WaitForTableBuild(context.Background(), "t")
			if err != nil {
				t.Fatal(err)
			}
			if res.Verdict != tt.want {
				t.Errorf("verdict = %q, want %q", res.Verdict, tt.want)
			}
			if !res.Derived {
				t.Error("a client-derived verdict must say so")
			}
		})
	}
}

// Treating an unknown status as still-running would hang on a finished build.
func TestWaitForTableBuildEndsOnAnUnrecognisedStatus(t *testing.T) {
	client := tableStates(t, `{"id":"t","status":"cancelled"}`)
	res, err := client.WaitForTableBuild(context.Background(), "t")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Verdict != "" || res.OK() {
		t.Errorf("result = %+v", res)
	}
}

// A table with no `code` parks at `pending` forever and the failure is recorded
// nowhere, so this deadline message is the only diagnosis.
func TestWaitForTableBuildNamesTheCodelessSyncCause(t *testing.T) {
	client := tableStates(t, `{"id":"table-1","status":"pending","buildVerdict":"pending"}`)
	client.TableBuild.Timeout = 5 * time.Millisecond

	_, err := client.WaitForTableBuild(context.Background(), "table-1")
	if err == nil {
		t.Fatal("a build stuck at pending must eventually be refused")
	}
	if !strings.Contains(err.Error(), "no `code`") {
		t.Errorf("the refusal must name the codeless-sync cause, got: %v", err)
	}
}

func TestWaitForTableBuildTimesOutOnASlowBuild(t *testing.T) {
	client := tableStates(t, `{"id":"table-1","status":"building","buildVerdict":"building"}`)
	client.TableBuild.Timeout = 5 * time.Millisecond

	last, err := client.WaitForTableBuild(context.Background(), "table-1")
	if err == nil {
		t.Fatal("expected a deadline error")
	}
	if strings.Contains(err.Error(), "no `code`") {
		t.Errorf("a building row is not the codeless case: %v", err)
	}
	if last == nil || last.Verdict != BuildVerdictBuilding {
		t.Errorf("last = %+v", last)
	}
}

// The failure budget is consecutive: a successful read starts it over.
func TestWaitForTableBuildSurvivesATransientFailure(t *testing.T) {
	var i int32
	client := fastPoll(serve(t, func(w http.ResponseWriter, _ *http.Request) {
		switch atomic.AddInt32(&i, 1) {
		case 1, 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"id":"t","status":"ready","buildVerdict":"ok"}`))
		}
	}))
	res, err := client.WaitForTableBuild(context.Background(), "t")
	if err != nil {
		t.Fatalf("two failures then a success must succeed: %v", err)
	}
	if res.Verdict != BuildVerdictOK {
		t.Errorf("result = %+v", res)
	}
}

func TestWaitForTableBuildGivesUpOnAnUnreachableInstance(t *testing.T) {
	client := fastPoll(serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	if _, err := client.WaitForTableBuild(context.Background(), "t"); err == nil {
		t.Fatal("expected a give-up error")
	} else if !strings.Contains(err.Error(), "lost contact") {
		t.Errorf("err = %v", err)
	}
}

func TestWaitForTableBuildStopsOnACancelledContext(t *testing.T) {
	client := tableStates(t, `{"id":"t","status":"building","buildVerdict":"building"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.WaitForTableBuild(ctx, "t"); err == nil {
		t.Fatal("a cancelled context must end the wait")
	}
}

// A zero Timeout must default, not mean "no deadline", or a caller who set only
// the interval would wait forever.
func TestTableBuildPollDefaultsEveryField(t *testing.T) {
	got := TableBuildPoll{Interval: time.Second}.withDefaults()
	if got.Interval != time.Second {
		t.Errorf("Interval = %s — an explicit value must survive", got.Interval)
	}
	if got.Timeout != DefaultTableBuildTimeout {
		t.Errorf("Timeout = %s, want the default — a zero is not 'wait forever'", got.Timeout)
	}
	if got.MaxInterval != DefaultMaxTableBuildPollInterval ||
		got.MaxFailureWindow != DefaultMaxTableBuildFailureWindow ||
		got.RateLimitBackoff != DefaultRateLimitBackoff {
		t.Errorf("defaults = %+v", got)
	}
}

// A 429 is transient but must back off: an unchanged cadence keeps producing the
// load being complained about.
func TestWaitForTableBuildBacksOffOnARateLimit(t *testing.T) {
	var i int32
	client := fastPoll(serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&i, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":"t","status":"ready","buildVerdict":"ok"}`))
	}))
	// MaxInterval caps the backoff too, so it is raised to make the bump visible.
	client.TableBuild.MaxInterval = 200 * time.Millisecond
	client.TableBuild.RateLimitBackoff = 20 * time.Millisecond

	start := time.Now()
	res, err := client.WaitForTableBuild(context.Background(), "t")
	if err != nil {
		t.Fatalf("a 429 must be retried, not fatal: %v", err)
	}
	if res.Verdict != BuildVerdictOK {
		t.Errorf("result = %+v", res)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("polled again after %s — the rate-limit backoff was not applied", elapsed)
	}
}
