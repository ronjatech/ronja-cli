package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// tagFixture is in rdb.Tag's full marshalled shape, including fields this
// mirror does not carry.
const tagFixture = `[
  {"tenantID":"tenant-1","id":"tag-fin","name":"Finance","color":"blue","description":"","spaceID":"space-1","createdAt":"2026-09-01T00:00:00Z"},
  {"tenantID":"tenant-1","id":"tag-kpi","name":"Board KPI","color":"gold","description":"x","spaceID":"space-1","createdAt":"2026-09-02T00:00:00Z"}
]`

func TestListTableTagsDecodesTheCatalogRow(t *testing.T) {
	var gotPath, gotMethod string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		writeString(w, tagFixture)
	})
	tags, err := client.ListTableTags(context.Background(), "table-aov")
	if err != nil {
		t.Fatalf("ListTableTags: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/api/v2/tag/of/table/table-aov" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if len(tags) != 2 || tags[0].ID != "tag-fin" || tags[0].Name != "Finance" || tags[1].Name != "Board KPI" {
		t.Errorf("tags = %+v", tags)
	}
}

func TestListTableTagsAnswersEmptyForNull(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) { writeString(w, `null`) })
	tags, err := client.ListTableTags(context.Background(), "table-aov")
	if err != nil {
		t.Fatalf("ListTableTags: %v", err)
	}
	if tags == nil || len(tags) != 0 {
		t.Errorf("tags = %#v, want an empty non-nil list", tags)
	}
}

func TestPatchTableTagsSendsOnlyTheListsItHas(t *testing.T) {
	var gotMethod, gotPath string
	var body map[string]json.RawMessage
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body is not JSON: %s", raw)
		}
		writeString(w, `{"tags":[{"id":"tag-fin","name":"Finance"},{"id":"tag-new","name":"Sales"}],
			"added":["Sales"],"removed":[],"unmatchedRemoves":["tag-gone"],"created":["Sales"]}`)
	})
	out, err := client.PatchTableTags(context.Background(), "table-aov",
		PatchTagsInput{Add: []string{"Sales"}, RemoveIDs: []string{"tag-gone"}})
	if err != nil {
		t.Fatalf("PatchTableTags: %v", err)
	}
	if gotMethod != "PATCH" || gotPath != "/api/v2/tag/of/table/table-aov" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if _, has := body["remove"]; has {
		t.Errorf("an empty remove list was sent: %v", body)
	}
	if string(body["add"]) != `["Sales"]` || string(body["removeIDs"]) != `["tag-gone"]` {
		t.Errorf("body = %v", body)
	}
	if len(out.Tags) != 2 || out.Tags[1].ID != "tag-new" {
		t.Errorf("tags = %+v", out.Tags)
	}
	if strings.Join(out.Added, ",") != "Sales" || strings.Join(out.UnmatchedRemoves, ",") != "tag-gone" ||
		strings.Join(out.Created, ",") != "Sales" {
		t.Errorf("result = %+v", out)
	}
}

// The metric loop reads a 404 as an instance older than the route.
func TestPatchTableTagsReportsAMissingRouteByStatus(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	})
	_, err := client.PatchTableTags(context.Background(), "table-aov", PatchTagsInput{Add: []string{"x"}})
	if StatusOf(err) != http.StatusNotFound {
		t.Errorf("status = %d (%v), want 404", StatusOf(err), err)
	}
}

func TestGetTableDecodesArchivedAndHidden(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeString(w, strings.Replace(strings.Replace(metricRowFixture,
			`"hidden": false`, `"hidden": true`, 1), `"archived": false`, `"archived": true`, 1))
	})
	row, err := client.GetTable(context.Background(), "table-aov")
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	if !row.Archived || !row.Hidden {
		t.Errorf("archived = %v, hidden = %v, want both true", row.Archived, row.Hidden)
	}
}
