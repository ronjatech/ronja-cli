package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The pipeline's live-first skips in `sync apply` beyond the two-run test: a
// draft published elsewhere and prose for a column not yet measured. The
// fixtures are sync_apply_idempotent_test.go's.

// webCommitTable is a colleague publishing the open draft of a table or metric
// in the web app: the draft lands on live and is gone.
func webCommitTable(t *testing.T, f *fakePipelineInstance, liveID string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	draftID := f.draftOf[liveID]
	if draftID == "" {
		t.Fatalf("no open draft of %s to publish", liveID)
	}
	rec := httptest.NewRecorder()
	f.serveCommit(rec, draftID, f.headVersionOf(liveID))
	if rec.Code != http.StatusOK {
		t.Fatalf("web publish of %s: %d %s", draftID, rec.Code, rec.Body)
	}
}

// A DRAFT PUBLISHED ELSEWHERE IS NOT A DRAFT TO PUBLISH. `pl push` stages an
// edit and records the draft; an admin publishes it in the web app; the apply
// that follows in the same checkout finds the live row already holding the
// folder and must neither write nor go after the draft it recorded. A pointer
// to a draft that is gone used to send the publish after it, and the folder
// came back refused with "1 of 1 draft(s) were not published".
//
// Three ways in: a push that finished (the file is not a target, so only the
// recorded pointer says anything — withGoneDraftPointers), a push whose build
// never finished (the file IS a target, and pushOneTable's LiveFirst skip has
// to drop the pointer itself), and a metric (whose publish asks the server
// rather than a pointer, pinned here so it stays that way).
func TestSyncApplyAfterAPipelineDraftWasPublishedElsewhere(t *testing.T) {
	cases := map[string]struct {
		liveID string
		stage  func(t *testing.T, fx idempotenceFixture)
		landed func(f *fakePipelineInstance) bool
	}{
		"table after a finished push": {
			liveID: "table-orders",
			stage: func(t *testing.T, fx idempotenceFixture) {
				writeLocal(t, fx.root, "orders.sql", "SELECT * FROM raw WHERE shipped")
				if out, stderr, err := runPipelineCLI(t, fx.root, "pipeline", "push"); err != nil {
					t.Fatalf("pipeline push: %v\n%s\n%s", err, out, stderr)
				}
			},
			landed: func(f *fakePipelineInstance) bool {
				return f.CodeOf("table-orders") == "SELECT * FROM raw WHERE shipped"
			},
		},
		"table after a push whose build failed": {
			liveID: "table-orders",
			stage: func(t *testing.T, fx idempotenceFixture) {
				fx.pipeline.failSync["table-draft-1"] = http.StatusInternalServerError
				writeLocal(t, fx.root, "orders.sql", "SELECT * FROM raw WHERE shipped")
				if _, _, err := runPipelineCLI(t, fx.root, "pipeline", "push"); err == nil {
					t.Fatal("pipeline push succeeded; the build was scripted to fail")
				}
				fx.pipeline.mu.Lock()
				delete(fx.pipeline.failSync, "table-draft-1")
				fx.pipeline.mu.Unlock()
			},
			landed: func(f *fakePipelineInstance) bool {
				return f.CodeOf("table-orders") == "SELECT * FROM raw WHERE shipped"
			},
		},
		"metric": {
			liveID: "metric-aov",
			stage: func(t *testing.T, fx idempotenceFixture) {
				writeLocal(t, fx.root, "metrics/aov.json", metricFileBody(
					`{"version":1,"value":"revenue / orders","source":"table-orders","base_measures":[{"name":"revenue","agg":"sum","column":"net_amount"}]}`))
				if out, stderr, err := runPipelineCLI(t, fx.root, "pipeline", "push"); err != nil {
					t.Fatalf("pipeline push: %v\n%s\n%s", err, out, stderr)
				}
			},
			landed: func(f *fakePipelineInstance) bool { return strings.Contains(f.RecipeOf("metric-aov"), "net_amount") },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := pipelineIdempotenceFixture(t)
			tc.stage(t, fx)
			webCommitTable(t, fx.pipeline, tc.liveID)
			if !tc.landed(fx.pipeline) {
				t.Fatal("the web publish did not land")
			}

			code, line, writes, out := applyOnce(t, fx)
			if code != syncExitClean || line.Verdict != verdictApplied {
				t.Fatalf("exit = %d, folder = %+v — a draft published elsewhere blocked the apply\n%s", code, line, out)
			}
			if len(writes) != 0 {
				t.Fatalf("apply sent %v for a folder live already holds", writes)
			}
			// And the pointer is gone, so the NEXT apply does not ask again.
			code, _, writes, out = applyOnce(t, fx)
			if code != syncExitClean || len(writes) != 0 {
				t.Fatalf("run 2: exit = %d, writes = %v\n%s", code, writes, out)
			}
		})
	}
}

// PROSE FOR A COLUMN THE TABLE DOES NOT HAVE YET IS STILL SENT. The server keeps
// it against the name and never reports it back (the join rule), so a live row
// cannot say whether it was ever written — and apply's live-first skip, asked
// under docsLanded's exemption, read a carrier whose only change was such a
// column as already deployed and never sent it.
//
// Both carriers. A .sql header is part of the SQL, so its change is usually a
// code change too; the skip decides only when the live SQL already holds the
// header — here a colleague published the same bytes in the web app without
// the prose ever being written.
func TestSyncApplySendsProseForAColumnNotYetMeasured(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	f := fx.pipeline
	f.mu.Lock()
	f.fields["table-orders"] = []string{"id"}
	f.mu.Unlock()
	writeLocal(t, fx.root, "orders.sql", "-- @table Orders.\n-- @column id: the order's id\nSELECT * FROM raw")
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean || line.Verdict != verdictApplied {
		t.Fatalf("run 1: exit = %d, folder = %+v\n%s", code, line, out)
	}

	withMargin := "-- @table Orders.\n-- @column id: the order's id\n-- @column margin: total less cost\nSELECT * FROM raw"
	writeLocal(t, fx.root, "orders.sql", withMargin)
	f.mu.Lock()
	f.tables["table-orders"].Code = withMargin
	f.mu.Unlock()
	writeSidecar(t, fx.root, "fortnox_invoices",
		`{"description": "One row per invoice line.", "columns": {"amount": "line total", "vat": "the VAT on the line"}}`)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean || line.Verdict != verdictApplied {
		t.Fatalf("run 2: exit = %d, folder = %+v\n%s", code, line, out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.columnDocs["table-orders"]["margin"]; got != "total less cost" {
		t.Errorf("the header's prose for a column not yet measured was never sent (stored %q)", got)
	}
	if got := f.columnDocs["table-raw"]["vat"]; got != "the VAT on the line" {
		t.Errorf("the sidecar's prose for a column not yet measured was never sent (stored %q)", got)
	}
}
