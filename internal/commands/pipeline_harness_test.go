package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The pipeline command harness.
//
// A SEPARATE fake from fakeInstance and fakeAppInstance rather than an extension
// of either — the repo's convention, and here it is not even close. The table
// surface diverges from both in four ways that these tests are entirely about:
//
//  1. CHECKOUT IS NOT IDEMPOTENT. A second checkout while a draft is open is an
//     error, where CheckoutWorkflow and the data app's simply return the draft.
//     Modelled, because the whole reason push reads GET :id/draft first is that
//     the alternative is a 400.
//  2. A SYNC BUILDS. The 200 means "accepted", every outcome is recorded on the
//     ROW, and a live table whose rebuild FAILED stays `ready` and goes on
//     serving its previous data — the trap buildVerdict exists to decode.
//  3. buildVerdict IS ONLY ON THE SINGLE-ROW GET. GET :id/draft goes through
//     toTableView, which does not compute it. That is why the CLI reads a draft's
//     verdict by the draft's own id, and a fake that stamped it on /draft would
//     let a CLI that skipped that read pass.
//  4. THE REVIEW ROUTES ARE ON ANOTHER PREFIX (/api/v2/table/draft/...) and in
//     another scope group, which is why "no confidence report" has to be a
//     survivable outcome rather than a failure.
type fakePipelineInstance struct {
	t *testing.T
	// mu guards everything below. UNLIKE the other two fakes, which is not
	// stylistic: `pipeline status` reads its tables CONCURRENTLY, so several
	// handler goroutines really are in here at once and an unguarded map write is
	// a genuine race, not a theoretical one.
	mu sync.Mutex

	// tables is every row, live and draft, keyed by id.
	tables map[string]*api.Table
	// draftOf maps a live table id to the caller's open draft id.
	draftOf map[string]string
	// buildErr is the failure recorded against a row, cleared at the start of
	// every sync exactly as the server clears its error rows.
	buildErr map[string]*api.TableBuildError
	// synced records which rows have ever been synced. The duckdb route asserts
	// against it: a draft that has been checked out and never synced has no
	// partitions of its own, so a sample read there is a bug in the CLI, not a
	// quirk of the fake.
	synced map[string]bool
	// fields is a row's column set, which is what the review payload's
	// fieldsDelta is computed from.
	//
	// It is also the MEASURED half of THE JOIN RULE: a documentation write
	// attaches only to a name in here, and one that is not is stored, reported
	// as unmatched, and left out of the row's `fields[]` entirely.
	fields map[string][]string
	// columnDocs is a row's column PROSE, keyed by row id and then by column
	// name — including names the row has not measured, which is the retention
	// half of the join rule: prose is kept against the name and re-attaches by
	// itself if a build later produces the column.
	columnDocs map[string]map[string]string
	// rowCounts is a row's materialized row count; an absent entry is -1, the
	// server's "not available", which is NOT zero.
	rowCounts map[string]int64
	// submitted records the drafts request-review has been called on, which is
	// what the review payload's submittedForReview reports. Modelled because the
	// CLI has to be able to tell "I am filing this" from "an admin has had it
	// since yesterday", and a payload that always said false would let a publish
	// report the second as the first.
	submitted map[string]bool
	// intervening is keyed by the LIVE table id: the versions that landed after a
	// draft forked, which is what makes baseStale true.
	intervening map[string][]api.TableInterveningVersion
	features    map[string]*api.Feature

	// syncOutcome scripts what a sync DOES to a row, keyed by the row id:
	// "" or "ready" (built), "build_failed", "failed_stale" (ready, and still
	// serving the previous data), "partial" (ready, incomplete data).
	syncOutcome map[string]string
	// syncMessage is the failure text a scripted failure records.
	syncMessage map[string]string
	// rejectCommit models the race the commit-timeout reconcile cannot see from
	// the draft alone: the draft is GONE when the reconcile looks because
	// somebody discarded it or a reviewer rejected it, NOT because this commit
	// landed. The parent is left exactly as it was.
	rejectCommit map[string]bool
	// syncBusy counts the pre-dispatch refusals a build already in flight earns:
	// a sync of this row — or of a draft of it — is answered 400 before anything
	// is dispatched, and the row is left `building`, which is where the rule
	// below picks it up.
	//
	// Modelled because it is the state an interrupted push leaves behind: Ctrl-C
	// stops the CLI listening, never the build.
	syncBusy map[string]int

	// stateAfterRead runs ONCE, immediately after GET :id/draft has been served,
	// and is how a test opens the window layer 1 exists to close: the CLI has
	// read the draft and compared it, and somebody else's edit lands before its
	// PUT arrives. Nothing the client-side guard does can see that — only the
	// precondition on the write can.
	stateAfterRead func()

	privilegeLevel   int
	noFrontendOrigin bool
	// failMe answers the organization lookup with a status instead of an
	// identity; 0 is off. See the field of the same name on fakeInstance.
	failMe int
	// listPageSize forces pagination; 0 answers everything in one page.
	listPageSize int
	// queryCSV is what POST /duckdb/query answers with.
	queryCSV string
	// queryError makes the query answer HTTP 200 with an `error` field — the trap
	// that endpoint is known for, and which must degrade to a note rather than
	// failing a push that has already succeeded.
	queryError string

	// --- failure injection, per row where it makes sense ---------------------
	failGet map[string]int
	// failDocsPut answers a DOCUMENTING PUT — one carrying `description` or
	// `fields` — with a status, leaving the SQL PUT of the same row alone.
	//
	// Its own knob rather than a reuse of failPut, because the case it models is
	// exactly the one the two-PUT split creates and no single-status fake can
	// reach: the SQL lands and builds, and only the prose is refused. That is
	// what a non-admin met on an instance whose allowlist had no `fields` in it,
	// and it is the state a push must be able to RETRY out of.
	failDocsPut  map[string]int
	failDraftGet map[string]int
	failCheckout map[string]int
	failPut      map[string]int
	failSync     map[string]int
	failCommit   map[string]int
	// nameTakenOnCommit scripts the OTHER 409 a commit can answer, keyed by
	// draft id: the draft would publish a name another live table in the feature
	// already holds. It is separate from failCommit because the BODY is the
	// point — the CLI must branch on the `code`, not on the 409 it shares with
	// the base-version CAS above.
	nameTakenOnCommit map[string]string
	failReview        map[string]int
	failCreate        int
	// nameTakenOnCreate is the sentence POST /feature/model answers 409
	// `name_taken` with; "" is off. Its own knob rather than a status on
	// failCreate because the BODY is what the CLI has to read.
	nameTakenOnCreate string
	// nameTakenOnMetricCreate is the same coded 409 on POST /feature/model/metric,
	// which is its own knob because the CLI's recovery there is DIFFERENT: a
	// table create reports the collision, a metric create looks the name up in
	// the feature and ADOPTS a metric it finds. "" is off.
	nameTakenOnMetricCreate string
	// failMetricCreate answers POST /feature/model/metric with a status; 0 is off.
	failMetricCreate int
	// failRecipePut answers PUT /feature/model/:id/metric/recipe with a status,
	// keyed by the DRAFT id. Its own knob rather than a reuse of failPut, because
	// the recipe route is a different handler with a different gate — a colleague's
	// draft is 403 there even for an admin.
	failRecipePut map[string]int
	failList      int
	failFeature   int
	failQuery     int
	// missTableStatus is what GET /feature/model/:id answers for a row this fake
	// does not hold. BOTH generations are real and this binary talks to both:
	// an older backend answers the lookup-miss sentinel `400 {"error":"no
	// rows"}`, a current one `404 {"error":"not found"}`. Defaults to the 400,
	// so a test that says nothing stages the older instance.
	missTableStatus int

	// --- what the fake was asked to do, for assertions -----------------------
	// metricsCreated and recipeWrites are the metric half's record of what was
	// asked, kept apart from `created`/`updates` because the bodies are entirely
	// different types and a shared slice would have to be one of them.
	metricsCreated  []api.CreateMetricInput
	recipeWrites    []recordedRecipeWrite
	created         []api.CreateTableInput
	updates         []recordedTableUpdate
	checkouts       []string
	syncs           []string
	committed       []string
	discarded       []string
	reviewRequested []string
	queries         []string
	listCalls       int
	nextID          int

	server *httptest.Server
	// Requests records every request served as "METHOD /path", in order. The
	// cheapest way to assert both that a command avoided a round trip and that it
	// made the ones it did in the right order — push's topological order above
	// all, which is invisible in the result.
	Requests []string
}

// recordedTableUpdate is one PUT, with the row it was ADDRESSED TO.
//
// The id is recorded because it is the one thing a write here can get silently
// wrong: writing the LIVE row instead of the draft is accepted, and then
// vanishes the moment anybody commits.
type recordedTableUpdate struct {
	ID          string
	Code        string
	InputModels []string
	// SetsCode and SetsInputModels report whether the PUT NAMED those keys at
	// all, which is not the same as what they held. The documentation write is a
	// PUT that names neither: rmodelv2.Patch reads every field as an optional, so
	// an absent `code` leaves the row's SQL alone — and a fake that read an
	// absent key as "" would model a server which WIPES the SQL of every table
	// the CLI documents, and would let that ship.
	SetsCode        bool
	SetsInputModels bool
	// The documentation half of a PUT. Description is a pointer for the
	// three-state rule: absent is unmanaged, and an explicit "" clears the row.
	Description       *string
	DescriptionSource string
	Fields            []api.TableFieldInput
	// BaseCodeSha256 is the precondition the PUT carried, as a POINTER so the
	// fake can tell ABSENT from PRESENT-AND-EMPTY. The server reads three states
	// and answers 400 to the empty one, so collapsing them here would let a
	// client that dropped `omitempty` — and therefore 400s every unconditional
	// write in production — pass every test.
	BaseCodeSha256 *string
}

// recordedRecipeWrite is one PUT to the metric-recipe route, with the row it was
// ADDRESSED TO.
//
// BaseRecipeSha256 and ReportingTimezone are POINTERS so the fake can tell
// ABSENT from PRESENT-AND-EMPTY. Both fields read three states server-side and
// answer differently to each — the precondition 400s on an empty string, and the
// timezone RESETS on one — so collapsing them here would let a client that
// dropped `omitempty` pass every test while 400ing (or silently resetting the
// calendar of) every push in production.
type recordedRecipeWrite struct {
	ID                string
	Recipe            string
	ReportingTimezone *string
	BaseRecipeSha256  *string
}

// recipeWriteBody is the recipe PUT's body as the SERVER sees it.
type recipeWriteBody struct {
	Recipe            json.RawMessage `json:"recipe"`
	ReportingTimezone *string         `json:"reportingTimezone"`
	BaseRecipeSha256  *string         `json:"baseRecipeSha256"`
}

// tableUpdateBody is the PUT body as the SERVER sees it, which is not quite
// api.UpdateTableInput: the precondition is a pointer here for the reason above.
type tableUpdateBody struct {
	Code           *string   `json:"code"`
	InputModels    *[]string `json:"inputModels"`
	BaseCodeSha256 *string   `json:"baseCodeSha256"`

	Description       *string               `json:"description"`
	DescriptionSource string                `json:"descriptionSource"`
	Fields            []api.TableFieldInput `json:"fields"`
}

func newFakePipelineInstance(t *testing.T) *fakePipelineInstance {
	t.Helper()
	f := &fakePipelineInstance{
		t:                 t,
		tables:            map[string]*api.Table{},
		draftOf:           map[string]string{},
		buildErr:          map[string]*api.TableBuildError{},
		synced:            map[string]bool{},
		fields:            map[string][]string{},
		columnDocs:        map[string]map[string]string{},
		rowCounts:         map[string]int64{},
		submitted:         map[string]bool{},
		intervening:       map[string][]api.TableInterveningVersion{},
		features:          map[string]*api.Feature{},
		syncOutcome:       map[string]string{},
		syncMessage:       map[string]string{},
		rejectCommit:      map[string]bool{},
		syncBusy:          map[string]int{},
		failGet:           map[string]int{},
		failDocsPut:       map[string]int{},
		failDraftGet:      map[string]int{},
		failCheckout:      map[string]int{},
		failPut:           map[string]int{},
		failSync:          map[string]int{},
		failCommit:        map[string]int{},
		nameTakenOnCommit: map[string]string{},
		failReview:        map[string]int{},
		failRecipePut:     map[string]int{},
		missTableStatus:   http.StatusBadRequest,
		privilegeLevel:    50,
		queryCSV:          "id,total\n1,42\n2,17\n",
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakePipelineInstance) URL() string { return f.server.URL }

func (f *fakePipelineInstance) Key() wfdir.InstanceKey {
	return wfdir.InstanceKey{URL: f.server.URL, TenantID: testTenantID}
}

// AddFeature registers a feature, which is what clone names the folder after and
// what publish reads the scope off.
func (f *fakePipelineInstance) AddFeature(id, name, scope string) {
	f.features[id] = &api.Feature{ID: id, Name: name, Scope: scope}
}

// AddTable registers a live row, filling in the fields every command reads so a
// test only has to state what it is actually about.
func (f *fakePipelineInstance) AddTable(table *api.Table) *api.Table {
	f.t.Helper()
	if table.Kind == "" {
		table.Kind = api.TableKindDerived
	}
	if table.FeatureID == "" {
		table.FeatureID = "collection-1"
	}
	if table.Status == "" {
		table.Status = api.TableStatusReady
	}
	if table.UpdatedAt.IsZero() {
		table.UpdatedAt = time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	}
	table.Normalize()
	f.tables[table.ID] = table
	f.synced[table.ID] = true
	return table
}

// AddDraft registers the caller's open draft of a live table, wired up the way
// the server does it: parentModelID points at the live row, shadowStatus is
// `draft`, and GET <live>/draft returns it.
func (f *fakePipelineInstance) AddDraft(liveID, draftID, code string) *api.Table {
	f.t.Helper()
	live := f.tables[liveID]
	if live == nil {
		f.t.Fatalf("AddDraft: no live table %s", liveID)
	}
	draft := f.AddTable(&api.Table{
		ID:            draftID,
		ParentModelID: liveID,
		ShadowStatus:  api.ShadowStatusDraft,
		Name:          live.Name,
		Kind:          live.Kind,
		FeatureID:     live.FeatureID,
		Code:          code,
		InputModels:   append([]string{}, live.InputModels...),
		UpdatedAt:     live.UpdatedAt.Add(time.Hour),
	})
	f.draftOf[liveID] = draftID
	return draft
}

// CodeOf reads one row's stored SQL back, for assertions.
func (f *fakePipelineInstance) CodeOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.tables[id]; row != nil {
		return row.Code
	}
	return ""
}

// countSyncs counts the syncs one row was actually DISPATCHED, which is not the
// same as the requests it received: a refused sync dispatches nothing.
func countSyncs(f *fakePipelineInstance, id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, synced := range f.syncs {
		if synced == id {
			n++
		}
	}
	return n
}

// requestsMatching counts the recorded requests whose "METHOD /path" contains a
// substring — the shape most count assertions here want.
func (f *fakePipelineInstance) requestsMatching(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.Requests {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

// requestOrder returns the recorded requests, for the order assertions.
func (f *fakePipelineInstance) requestOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.Requests...)
}

// verdictOf mirrors feature.tableBuildVerdict — the ROW-SCOPED computation the
// single-row GET performs. Modelled rather than stubbed, because the whole
// `ready`-with-an-error trap lives in it.
func (f *fakePipelineInstance) verdictOf(row *api.Table) string {
	buildErr := f.buildErr[row.ID]
	switch row.Status {
	case api.TableStatusBuilding:
		return api.BuildVerdictBuilding
	case api.TableStatusPending:
		return api.BuildVerdictPending
	case api.TableStatusInvalidated:
		return api.BuildVerdictInvalidated
	case api.TableStatusBuildFailed:
		return api.BuildVerdictFailed
	case api.TableStatusReady:
		switch {
		case buildErr == nil:
			return api.BuildVerdictOK
		case buildErr.Type == api.PartialDataCorruptErrorType:
			return api.BuildVerdictOKPartial
		default:
			return api.BuildVerdictFailedStale
		}
	}
	return ""
}

// writeRow answers a SINGLE-ROW route the way the real handler does: the stored
// row, plus the two fields that are opt-in on that route only (buildVerdict and
// lastBuildError) and the `url` its view stamps on a frontend origin that is not
// the API's — see fakeFrontendOrigin.
func (f *fakePipelineInstance) writeRow(w http.ResponseWriter, row *api.Table) {
	if row == nil {
		writeJSON(w, nil)
		return
	}
	out := *row
	out.BuildVerdict = f.verdictOf(row)
	out.LastBuildError = f.buildErr[row.ID]
	if !f.noFrontendOrigin {
		out.URL = fakeFrontendOrigin + "/tables/" + out.ID
	}
	writeJSON(w, out)
}

// writeRowWithCatalog answers the SINGLE-ROW GET, which is the ONE response that
// carries `fields[]`. Modelled as its own method because that asymmetry is real
// and load-bearing: the create, the checkout and the draft read all share the
// DTO and none of them populates the catalog, so a CLI that took a documentation
// fingerprint from one of those would be hashing a table with no columns. A fake
// that filled the field in everywhere would hide exactly that bug.
func (f *fakePipelineInstance) writeRowWithCatalog(w http.ResponseWriter, row *api.Table) {
	if row == nil {
		writeJSON(w, nil)
		return
	}
	out := *row
	out.Fields = f.columnCatalog(row.ID)
	f.writeRow(w, &out)
}

// columnCatalog is THE JOIN RULE on the read side: the columns the row has
// MEASURED, each carrying whatever prose documents it. A documented name the
// row has not measured is NOT here, however recently somebody wrote it —
// documenting a column cannot bring it into existence.
func (f *fakePipelineInstance) columnCatalog(id string) []api.TableField {
	measured := f.fields[id]
	if len(measured) == 0 {
		return nil
	}
	docs := f.columnDocs[id]
	names := append([]string{}, measured...)
	sortStrings(names)
	out := make([]api.TableField, 0, len(names))
	for _, name := range names {
		field := api.TableField{Name: name, Description: docs[name]}
		if field.Description != "" {
			field.DescriptionSource = api.DescriptionSourceUser
		}
		out = append(out, field)
	}
	return out
}

// documentColumns stores a `fields` write and answers THE JOIN RULE: which of
// the names given attached to a measured column and which did not.
//
// The unmatched prose IS STORED, which is the retention half — a transient
// teardown or a renamed staging column must not destroy what somebody wrote, and
// the prose re-attaches by itself the moment a build produces the column.
func (f *fakePipelineInstance) documentColumns(id string, fields []api.TableFieldInput) *api.ColumnDocReport {
	if fields == nil {
		return nil
	}
	measured := map[string]bool{}
	for _, name := range f.fields[id] {
		measured[name] = true
	}
	if f.columnDocs[id] == nil {
		f.columnDocs[id] = map[string]string{}
	}
	var report api.ColumnDocReport
	seen := map[string]bool{}
	for _, field := range fields {
		name := strings.TrimSpace(field.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		f.columnDocs[id][name] = field.Description
		if measured[name] {
			report.AttachedColumns = append(report.AttachedColumns, name)
		} else {
			report.UnmatchedColumns = append(report.UnmatchedColumns, name)
		}
	}
	sortStrings(report.AttachedColumns)
	sortStrings(report.UnmatchedColumns)
	return &report
}

// copyColumnDocs is the fake's copyFields: checkout copies live → draft, commit
// copies draft → live. Without it a draft would be documented and the commit
// would leave the live row saying nothing, which is the opposite of what the
// server does and would make the CLI's "write to the draft" rule look wrong.
func (f *fakePipelineInstance) copyColumnDocs(fromID, toID string) {
	if src := f.columnDocs[fromID]; len(src) > 0 {
		dst := map[string]string{}
		for name, text := range src {
			dst[name] = text
		}
		f.columnDocs[toID] = dst
	}
	if src := f.fields[fromID]; len(src) > 0 && len(f.fields[toID]) == 0 {
		f.fields[toID] = append([]string{}, src...)
	}
}

// writeDraftRow answers GET :id/draft, which goes through toTableView and
// therefore carries NO buildVerdict and NO lastBuildError.
//
// The omission is modelled deliberately: it is the reason the CLI reads a
// draft's verdict by the draft's own id, and a fake that filled the field in
// would hide a CLI that never made that read.
func (f *fakePipelineInstance) writeDraftRow(w http.ResponseWriter, row *api.Table) {
	if row == nil {
		writeJSON(w, nil)
		return
	}
	out := *row
	if !f.noFrontendOrigin {
		out.URL = fakeFrontendOrigin + "/tables/" + out.ID
	}
	writeJSON(w, out)
}

func (f *fakePipelineInstance) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/api/v2/authentication/me":
		// The organization lookup, failing the way a revoked token makes it fail:
		// `pipeline status` must degrade to a local report rather than abort.
		if f.failMe != 0 {
			http.Error(w, `{"error":"no"}`, f.failMe)
			return
		}
		writeJSON(w, map[string]any{
			"user":   map[string]any{"id": "usr-1", "email": "dev@example.com"},
			"role":   map[string]any{"name": "user", "privilegeLevel": f.privilegeLevel},
			"tenant": map[string]any{"id": testTenantID, "name": "Test Org"},
		})
		return
	case r.URL.Path == "/api/v2/duckdb/query":
		f.serveQuery(w, r)
		return
	}

	// The governance prefix. Checked BEFORE /feature/model, because these two
	// routes live on a different handler in a different scope group and a fake
	// that folded them together would hide exactly the failure the CLI has to
	// survive: a token that runs the whole loop except the review read.
	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v2/table/draft/"); ok {
		f.serveDraftGovernance(w, rest)
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v2/feature/model"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		f.serveModel(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if featureID, ok := strings.CutPrefix(r.URL.Path, "/api/v2/feature/"); ok {
		if f.failFeature != 0 {
			http.Error(w, `{"error":"boom"}`, f.failFeature)
			return
		}
		feature, known := f.features[featureID]
		if !known {
			// The REAL route's wording. A feature this caller cannot reach and
			// one that does not exist are deliberately one answer on the server
			// (RLS), and the CLI recognises it by code — so a fake that invented
			// its own "not found" would let `pipeline init` pass a test while
			// falling through to the raw wrapper against a live instance.
			http.Error(w, `{"error":"feature not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, feature)
		return
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

func (f *fakePipelineInstance) serveModel(w http.ResponseWriter, r *http.Request, rest string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			f.serveList(w, r)
		case http.MethodPost:
			f.serveCreate(w, r)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
		return
	}
	// POST "/metric" is a STATIC sibling of the "/:id" param child, exactly as
	// POST "/draft" already is on the real router — so it is matched before the
	// id split below, or "metric" would be read as a table id.
	if rest == "metric" && r.Method == http.MethodPost {
		f.serveCreateMetric(w, r)
		return
	}

	id, action, _ := strings.Cut(rest, "/")
	switch {
	case action == "draft" && r.Method == http.MethodGet:
		if status := f.failDraftGet[id]; status != 0 {
			http.Error(w, `{"error":"boom"}`, status)
			return
		}
		draftID, has := f.draftOf[id]
		if !has {
			// The shape that matters: 200 with a null body, not a 404.
			writeJSON(w, nil)
			return
		}
		f.writeDraftRow(w, f.tables[draftID])
		// AFTER the answer is written, so the CLI really does hold the row as it
		// was before the hook moved it. Once only.
		if hook := f.stateAfterRead; hook != nil {
			f.stateAfterRead = nil
			hook()
		}

	case action == "metric/recipe" && r.Method == http.MethodPut:
		f.serveWriteRecipe(w, r, id)

	case action == "checkout" && r.Method == http.MethodPost:
		f.serveCheckout(w, id)

	case action == "sync" && r.Method == http.MethodPost:
		f.serveSync(w, id)

	case action == "commit" && r.Method == http.MethodPost:
		var in api.TableCommitDraftInput
		decodeOptionalBody(f.t, r, &in)
		f.serveCommit(w, id, in.ConfirmHeadVersionID)

	case action == "discard" && r.Method == http.MethodPost:
		draft := f.tables[id]
		if draft == nil || draft.ShadowStatus != api.ShadowStatusDraft {
			http.Error(w, `{"error":"not a draft"}`, http.StatusBadRequest)
			return
		}
		f.discarded = append(f.discarded, id)
		f.dropDraft(id)
		writeJSON(w, nil)

	case action == "" && r.Method == http.MethodPut:
		if status := f.failPut[id]; status != 0 {
			http.Error(w, `{"error":"boom"}`, status)
			return
		}
		row := f.tables[id]
		if row == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		var in tableUpdateBody
		decodeBody(f.t, r, &in)
		if status := f.failDocsPut[id]; status != 0 && (in.Fields != nil || in.Description != nil) {
			// The message production sends a non-admin, verbatim: the CLI must
			// explain it from the STATUS, never by reading this prose.
			http.Error(w, `{"error":"field not allowed: fields"}`, status)
			return
		}
		recorded := recordedTableUpdate{
			ID: id, BaseCodeSha256: in.BaseCodeSha256,
			SetsCode: in.Code != nil, SetsInputModels: in.InputModels != nil,
			Description: in.Description, DescriptionSource: in.DescriptionSource, Fields: in.Fields,
		}
		if in.Code != nil {
			recorded.Code = *in.Code
		}
		if in.InputModels != nil {
			recorded.InputModels = *in.InputModels
		}
		f.updates = append(f.updates, recorded)
		// The layer-1 precondition, modelled the way rmodelv2.checkCodePrecondition
		// decides it and NOT the way the CLI intends it: the digest is compared
		// against what THIS ROW STORES, whatever the client believed it was
		// fingerprinting. That is the whole point of the fake refusing here — a
		// client that sent the disk-form hash of an aliased file would be caught
		// by production and must be caught here too.
		//
		// Three states: absent is no check, "" is a 400 rather than a silent
		// no-check, and a digest is compare-and-swap.
		if in.BaseCodeSha256 != nil {
			if *in.BaseCodeSha256 == "" {
				http.Error(w, `{"error":"baseCodeSha256 must be a 64-character lowercase hex sha256"}`, http.StatusBadRequest)
				return
			}
			if wfdir.HashString(row.Code) != *in.BaseCodeSha256 {
				http.Error(w, `{"error":"this table's code changed since you last read it"}`, http.StatusConflict)
				return
			}
		}
		// EVERY FIELD IS AN OPTIONAL, exactly as rmodelv2.Patch is: an absent key
		// leaves the column alone. Modelled rather than collapsed because the
		// documentation write is a PUT that names no `code`, and a fake that read
		// that as "" would model a server which wipes the SQL of every table the
		// CLI documents — and would let that ship.
		//
		// Stored VERBATIM on the row that was addressed, which is the other
		// property worth modelling: a PUT to the live row is accepted and is
		// silently lost work, so nothing here redirects it to a draft.
		if in.Code != nil {
			row.Code = *in.Code
		}
		if in.InputModels != nil {
			row.InputModels = *in.InputModels
		}
		if in.Description != nil {
			row.Description = *in.Description
			row.DescriptionSource = in.DescriptionSource
		}
		row.UpdatedAt = row.UpdatedAt.Add(time.Minute)
		if in.Fields == nil {
			// A PUT that documents nothing answers exactly what it always did.
			writeJSON(w, nil)
			return
		}
		writeJSON(w, f.documentColumns(id, in.Fields))

	case action == "" && r.Method == http.MethodGet:
		if status := f.failGet[id]; status != 0 {
			http.Error(w, `{"error":"boom"}`, status)
			return
		}
		row := f.tables[id]
		if row == nil {
			// A row the caller cannot see is invisible to RLS, so the read finds
			// nothing and cannot tell "not yours" from "not there" — which is the
			// point. WHICH STATUS that sentinel reaches the wire as depends on the
			// instance: an older backend answers 400 {"error":"no rows"}, a
			// current one 404 {"error":"not found"}, and the CLI ships against
			// both — hence missTableStatus, and hence a twin test per arm.
			//
			// This fake said 404 once when every backend answered 400, and
			// explainUnreadableRefs was written against that fiction: its test
			// passed while the real cross-organization case fell through to the
			// wrong diagnosis. Stage what a real instance answers, both of them.
			if f.missTableStatus == http.StatusNotFound {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"no rows"}`, http.StatusBadRequest)
			return
		}
		f.writeRowWithCatalog(w, row)
		// A row left `building` — which only a refused sync does here — lands on
		// the read that watched it. Reported first and landed after, so the caller
		// that asked BECAUSE it was refused sees the build it was told about.
		if row.Status == api.TableStatusBuilding {
			f.synced[id] = true
			f.applyBuildOutcome(id)
		}

	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// serveList mirrors the LIST projection: no `code`, no `buildVerdict`, shadow
// rows excluded, and offset pagination whose token is a string.
func (f *fakePipelineInstance) serveList(w http.ResponseWriter, r *http.Request) {
	f.listCalls++
	if f.failList != 0 {
		http.Error(w, `{"error":"boom"}`, f.failList)
		return
	}
	featureID := r.URL.Query().Get("featureID")
	var items []*api.TableListItem
	for _, id := range f.sortedIDs() {
		row := f.tables[id]
		if row.FeatureID != featureID || row.ShadowStatus != "" || row.ParentModelID != "" {
			continue
		}
		items = append(items, &api.TableListItem{
			ID: row.ID, FeatureID: row.FeatureID, Name: row.Name, Kind: row.Kind,
			Status: row.Status, HasError: f.buildErr[row.ID] != nil, UpdatedAt: row.UpdatedAt,
		})
	}

	offset := 0
	if token := r.URL.Query().Get("token"); token != "" {
		offset, _ = strconv.Atoi(token)
	}
	page := items
	next := ""
	size := f.listPageSize
	if size > 0 {
		if offset > len(items) {
			offset = len(items)
		}
		end := offset + size
		if end > len(items) {
			end = len(items)
		}
		page = items[offset:end]
		if end < len(items) {
			next = strconv.Itoa(end)
		}
	}
	if page == nil {
		page = []*api.TableListItem{}
	}
	writeJSON(w, map[string]any{"token": next, "total": len(items), "result": page})
}

func (f *fakePipelineInstance) serveCreate(w http.ResponseWriter, r *http.Request) {
	if f.nameTakenOnCreate != "" {
		http.Error(w, `{"error":`+strconv.Quote(f.nameTakenOnCreate)+`,"code":"name_taken"}`, http.StatusConflict)
		return
	}
	if f.failCreate != 0 {
		http.Error(w, `{"error":"admin required to create a table"}`, f.failCreate)
		return
	}
	var in api.CreateTableInput
	decodeBody(f.t, r, &in)
	f.created = append(f.created, in)
	f.nextID++
	// Created LIVE, not as a draft — unlike a workflow or a data app, whose
	// create endpoints mint a parentless draft. That is the whole reason push
	// records the binding before it does anything else.
	row := f.AddTable(&api.Table{
		ID:                fmt.Sprintf("table-new-%d", f.nextID),
		Name:              in.Name,
		Kind:              in.Kind,
		FeatureID:         in.FeatureID,
		Code:              in.Code,
		InputModels:       in.InputModels,
		Status:            api.TableStatusPending,
		Description:       in.Description,
		DescriptionSource: in.DescriptionSource,
	})
	f.synced[row.ID] = false
	// The report rides the create response, embedded — and on a create it is
	// ALWAYS every name unmatched, because a table that has never built has
	// measured nothing. That is the join rule, not a special case.
	report := f.documentColumns(row.ID, in.Fields)
	out := *row
	if report != nil {
		out.ColumnDocReport = *report
	}
	f.writeRow(w, &out)
}

// serveCheckout models the ONE lifecycle divergence that shapes the whole push
// path: a second checkout while a draft is open is an ERROR, not the existing
// draft.
func (f *fakePipelineInstance) serveCheckout(w http.ResponseWriter, liveID string) {
	if status := f.failCheckout[liveID]; status != 0 {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}
	live := f.tables[liveID]
	if live == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if _, exists := f.draftOf[liveID]; exists {
		http.Error(w, `{"error":"you already have an open draft of this table"}`, http.StatusBadRequest)
		return
	}
	f.checkouts = append(f.checkouts, liveID)
	f.nextID++
	draftID := fmt.Sprintf("table-draft-%d", f.nextID)
	// The draft COPIES the live row's config and its data references, which is
	// what makes it queryable before anything changes — but it has no partitions
	// of its own until it is synced, and the server stamps it `pending` to say so
	// (store_shadow.go). NOT the parent's status: a checked-out draft of a healthy
	// `ready` table that inherited `ready` would read as a built draft, and every
	// command that asks "has this been built?" — publish above all — would answer
	// yes about a row nothing has ever built.
	draft := f.AddTable(&api.Table{
		ID:            draftID,
		ParentModelID: liveID,
		ShadowStatus:  api.ShadowStatusDraft,
		Name:          live.Name,
		Kind:          live.Kind,
		FeatureID:     live.FeatureID,
		Code:          live.Code,
		InputModels:   append([]string{}, live.InputModels...),
		Status:        api.TableStatusPending,
		UpdatedAt:     live.UpdatedAt.Add(time.Minute),
	})
	f.synced[draftID] = false
	f.draftOf[liveID] = draftID
	draft.Description = live.Description
	draft.DescriptionSource = live.DescriptionSource
	// The metric axis rides the checkout too: a draft of a metric INHERITS its
	// parent's recipe, which is what makes a timezone-only write expressible and
	// what the CLI's baseRecipeSha256 is taken over on a resumed draft.
	draft.MetricRecipe = live.MetricRecipe
	draft.MetricStatus = live.MetricStatus
	f.copyColumnDocs(liveID, draftID)
	f.writeRow(w, draft)
}

// serveSync models the accepted-not-succeeded contract: a 200 that says only
// that the build was dispatched, with every outcome recorded on the ROW.
func (f *fakePipelineInstance) serveSync(w http.ResponseWriter, id string) {
	if status := f.failSync[id]; status != 0 {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}
	row := f.tables[id]
	if row == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// Refused BEFORE anything is dispatched, and the row goes on building — the
	// server's own pre-dispatch guard, whose whole point is that the 200 it would
	// otherwise return describes a build that was silently dropped deeper in the
	// stack.
	if key := f.busyKey(id); key != "" {
		f.syncBusy[key]--
		row.Status = api.TableStatusBuilding
		http.Error(w, `{"error":"a build of this table is already running"}`, http.StatusBadRequest)
		return
	}
	f.syncs = append(f.syncs, id)
	f.synced[id] = true
	// Cleared at the start of every rebuild, so a present error always describes
	// the LATEST build.
	delete(f.buildErr, id)
	f.applyBuildOutcome(id)
	row.UpdatedAt = row.UpdatedAt.Add(time.Minute)
	writeJSON(w, nil)
}

// busyKey names the syncBusy entry covering a row — its own, or its parent's, so
// a test can script a draft the CLI has not checked out yet.
func (f *fakePipelineInstance) busyKey(id string) string {
	if f.syncBusy[id] > 0 {
		return id
	}
	if row := f.tables[id]; row != nil && row.ParentModelID != "" && f.syncBusy[row.ParentModelID] > 0 {
		return row.ParentModelID
	}
	return ""
}

// applyBuildOutcome puts the scripted result of a build onto the row.
func (f *fakePipelineInstance) applyBuildOutcome(id string) {
	row := f.tables[id]
	if row == nil {
		return
	}
	// Scripted by the row's own id, falling back to its PARENT's. The fallback is
	// what lets a test say "this table's build fails" without predicting the id
	// of a draft the CLI has not checked out yet — which would make every such
	// test depend on the fake's id counter rather than on the behaviour.
	outcome, message := f.syncOutcome[id], f.syncMessage[id]
	if row.ParentModelID != "" {
		if outcome == "" {
			outcome = f.syncOutcome[row.ParentModelID]
		}
		if message == "" {
			message = f.syncMessage[row.ParentModelID]
		}
	}
	if message == "" {
		message = "The transformation is impossible with the given inputs: `orders` has no column `customer_id`."
	}
	// A build MEASURES the columns. A draft that has never been built has none of
	// its own, so it inherits its parent's — which is what lets a test declare
	// the measured set once, on the live table, without predicting the id of a
	// draft the CLI has not checked out yet.
	if row.ParentModelID != "" && len(f.fields[id]) == 0 {
		f.fields[id] = append([]string{}, f.fields[row.ParentModelID]...)
	}
	switch outcome {
	case "build_failed":
		row.Status = api.TableStatusBuildFailed
		f.buildErr[id] = &api.TableBuildError{Message: message, Type: "impossible"}
	case "failed_stale":
		// The trap: `ready`, and an error. The table is serving its PREVIOUS data.
		row.Status = api.TableStatusReady
		f.buildErr[id] = &api.TableBuildError{Message: message, Type: "impossible"}
	case "partial":
		row.Status = api.TableStatusReady
		f.buildErr[id] = &api.TableBuildError{Message: "some source files were corrupt", Type: api.PartialDataCorruptErrorType}
	default:
		row.Status = api.TableStatusReady
	}
}

func (f *fakePipelineInstance) serveCommit(w http.ResponseWriter, draftID, confirmHeadVersionID string) {
	if status := f.failCommit[draftID]; status != 0 {
		http.Error(w, `{"error":"admin required to commit into a shared feature"}`, status)
		return
	}
	// Answered BEFORE the base-version CAS below, so the test exercises the two
	// 409s as the distinct refusals they are rather than whichever the fake
	// happens to reach first.
	if why := f.nameTakenOnCommit[draftID]; why != "" {
		http.Error(w, `{"error":`+strconv.Quote(why)+`,"code":"name_taken"}`, http.StatusConflict)
		return
	}
	draft := f.tables[draftID]
	if draft == nil || draft.ParentModelID == "" {
		http.Error(w, `{"error":"not a draft"}`, http.StatusBadRequest)
		return
	}
	// THE GOVERNANCE GATE, modelled because it is a property of the SERVER and
	// not of the CLI: committing into a SHARED feature is admin-only, and the
	// refusal is a 400 — which is exactly the status the publish loop reads as
	// "the needs-review case" and, without --no-request-review, converts into a
	// review request it then reports as a SUCCESS.
	//
	// A fake that let every commit through modelled the CLI's intent instead: a
	// non-admin run against a shared feature came back "published" here and filed
	// thirty review requests in production. Privilege levels count DOWN, so 10 is
	// USR_ADMIN.
	if feature := f.features[draft.FeatureID]; feature != nil &&
		feature.Scope == scopeOrganization && f.privilegeLevel > 10 {
		http.Error(w, `{"error":"admin required to commit into a shared feature"}`, http.StatusBadRequest)
		return
	}
	// The base-version CAS, read off the SAME state that makes the review
	// payload's baseStale true — one source of truth in the fake, because it is
	// one in the server: the gate and the payload both resolve the head through
	// rmodelv2.lastCommittedShadow. A stale draft is REFUSED, and nothing is
	// written; the confirm must equal the CURRENT head, not merely be non-empty.
	if head := f.headVersionOf(draft.ParentModelID); head != "" && confirmHeadVersionID != head {
		http.Error(w, `{"error":"this table changed since your draft was created — it is now at version `+head+`"}`, http.StatusConflict)
		return
	}
	// A backstop the CLI should never reach: it refuses a failed draft itself,
	// naming the fix. Modelled so that a CLI which stopped doing so fails here
	// rather than putting a table live with nothing behind it.
	if draft.Status == api.TableStatusBuildFailed {
		http.Error(w, `{"error":"this draft did not build"}`, http.StatusBadRequest)
		return
	}
	if f.rejectCommit[draftID] {
		f.dropDraft(draftID)
		writeJSON(w, nil)
		return
	}
	f.committed = append(f.committed, draftID)
	// The commit landed, so the parent is now at the version this draft made:
	// nothing is intervening any more. Modelled, or a second publish of the same
	// folder would be refused against a version it had just overwritten.
	delete(f.intervening, draft.ParentModelID)
	parent := f.tables[draft.ParentModelID]
	if parent != nil {
		parent.Code = draft.Code
		parent.InputModels = append([]string{}, draft.InputModels...)
		parent.Status = draft.Status
		parent.UpdatedAt = draft.UpdatedAt.Add(time.Minute)
		// The commit overwrites the parent's fields FROM THE DRAFT, and copyFields
		// carries the column rows with them — which is what makes writing prose
		// into a draft the right thing to do rather than a way of losing it.
		parent.Description = draft.Description
		parent.DescriptionSource = draft.DescriptionSource
		// The recipe is copied with everything else, and metricStatus is NOT: a
		// commit never verifies or unverifies. That is precisely why a recipe
		// change lands DRIFTED — the definition moved and the verification did
		// not — which is the one governance consequence the metric loop reports.
		parent.MetricRecipe = draft.MetricRecipe
		f.copyColumnDocs(draftID, parent.ID)
		f.synced[parent.ID] = true
	}
	f.dropDraft(draftID)
	writeJSON(w, nil)
}

func (f *fakePipelineInstance) dropDraft(draftID string) {
	draft := f.tables[draftID]
	if draft == nil {
		return
	}
	delete(f.draftOf, draft.ParentModelID)
	delete(f.tables, draftID)
	delete(f.buildErr, draftID)
	delete(f.synced, draftID)
}

// serveDraftGovernance answers the two routes on the governance handler:
// GET /table/draft/:id/review and POST /table/draft/:id/request-review.
func (f *fakePipelineInstance) serveDraftGovernance(w http.ResponseWriter, rest string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, action, _ := strings.Cut(rest, "/")
	switch action {
	case "request-review":
		f.reviewRequested = append(f.reviewRequested, id)
		f.submitted[id] = true
		writeJSON(w, nil)
	case "review":
		if status := f.failReview[id]; status != 0 {
			http.Error(w, `{"error":"forbidden"}`, status)
			return
		}
		draft := f.tables[id]
		if draft == nil || draft.ParentModelID == "" {
			http.Error(w, `{"error":"not a draft"}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, f.reviewPayload(draft))
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// reviewPayload computes the diff from the fake's own state, rather than
// returning a canned blob: the CLI renders row counts and a field delta, and a
// constant payload would let a report that mixed up draft and live pass.
func (f *fakePipelineInstance) reviewPayload(draft *api.Table) api.TableDraftReview {
	live := f.tables[draft.ParentModelID]
	out := api.TableDraftReview{
		DraftID:       draft.ID,
		TableID:       draft.ParentModelID,
		FieldsDelta:   []api.TableFieldDelta{},
		DraftRowCount: f.rowCountOf(draft.ID),
		LiveRowCount:  f.rowCountOf(draft.ParentModelID),

		SubmittedForReview: f.submitted[draft.ID],
	}
	liveFields := map[string]bool{}
	for _, name := range f.fields[draft.ParentModelID] {
		liveFields[name] = true
	}
	draftFields := map[string]bool{}
	for _, name := range f.fields[draft.ID] {
		draftFields[name] = true
	}
	for _, name := range f.fields[draft.ID] {
		if !liveFields[name] {
			out.FieldsDelta = append(out.FieldsDelta, api.TableFieldDelta{Name: name, Change: "added"})
		}
	}
	for _, name := range f.fields[draft.ParentModelID] {
		if !draftFields[name] {
			out.FieldsDelta = append(out.FieldsDelta, api.TableFieldDelta{Name: name, Change: "removed"})
		}
	}
	// The lineage delta, as a SET comparison — which is what the server does,
	// and computing it here from the rows themselves is what makes a report that
	// mixed draft and live up fail.
	lineage := api.TableInputModelsDelta{}
	liveInputs := map[string]bool{}
	if live != nil {
		for _, id := range live.InputModels {
			liveInputs[id] = true
		}
	}
	draftInputs := map[string]bool{}
	for _, id := range draft.InputModels {
		draftInputs[id] = true
	}
	for _, id := range draft.InputModels {
		if !liveInputs[id] {
			lineage.Added = append(lineage.Added, id)
		}
	}
	if live != nil {
		for _, id := range live.InputModels {
			if !draftInputs[id] {
				lineage.Removed = append(lineage.Removed, id)
			}
		}
	}
	// ABSENT when they agree, exactly as the omitempty pointer on the wire.
	if len(lineage.Added) > 0 || len(lineage.Removed) > 0 {
		out.InputModelsDelta = &lineage
	}
	if versions := f.intervening[draft.ParentModelID]; len(versions) > 0 {
		out.BaseStale = true
		out.InterveningVersions = versions
	}
	// Returned ALWAYS, not only when baseStale — that is the server's contract,
	// and it is what lets a caller carry the head into a commit without a second
	// round trip. Empty when the table has never been committed to.
	out.HeadVersionID = f.headVersionOf(draft.ParentModelID)
	return out
}

// headVersionOf is the fake's head-of-version-history for a LIVE table: the
// newest intervening version, since `intervening` is what this fake uses to say
// "this table has moved since the drafts of it forked". Empty means the table
// has never been committed to, which is a real state and not "unknown" — the
// server returns empty there too rather than falling back to the table's own id.
//
// intervening is newest-first, matching the server's committed_at DESC ordering.
func (f *fakePipelineInstance) headVersionOf(liveID string) string {
	versions := f.intervening[liveID]
	if len(versions) == 0 {
		return ""
	}
	return versions[0].VersionID
}

func (f *fakePipelineInstance) rowCountOf(id string) int64 {
	if n, known := f.rowCounts[id]; known {
		return n
	}
	// -1 is the server's "not available", and it is NOT zero.
	return -1
}

// serveQuery answers the sample read, and ASSERTS the precondition the CLI is
// supposed to respect: a draft that has never been synced has no partitions of
// its own, so a sample read there would fail in production with an error about
// nothing.
func (f *fakePipelineInstance) serveQuery(w http.ResponseWriter, r *http.Request) {
	var in api.QueryInput
	decodeBody(f.t, r, &in)

	f.mu.Lock()
	f.queries = append(f.queries, in.SQL)
	status := f.failQuery
	csv, queryErr := f.queryCSV, f.queryError
	for id, wasSynced := range f.synced {
		if strings.Contains(in.SQL, "'"+id+"'") && !wasSynced {
			f.t.Errorf("sample query read %s, which has never been synced: %s", id, in.SQL)
		}
	}
	f.mu.Unlock()

	if status != 0 {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}
	// A FAILED query is HTTP 200 with the failure in a field, which is the trap
	// this endpoint is known for.
	writeJSON(w, api.QueryResult{Result: csv, RowCount: 2, Error: queryErr, SQL: in.SQL})
}

func (f *fakePipelineInstance) sortedIDs() []string {
	out := make([]string, 0, len(f.tables))
	for id := range f.tables {
		out = append(out, id)
	}
	sortStrings(out)
	return out
}

// sortStrings is sort.Strings, wrapped so this file needs no sort import beside
// everything else it already carries.
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// --- folder helpers ---------------------------------------------------------

// signInPipeline points the CLI at a fake pipeline instance with a credential in
// an isolated config dir.
func signInPipeline(t *testing.T, f *fakePipelineInstance) {
	t.Helper()
	signInTo(t, f.URL())
}

// writePipelineFolder lays out a pipeline folder on disk: a manifest of the
// right kind, an optional binding, and the given files.
//
// The baseline is written separately by the tests that need one, because "has no
// baseline" is itself a state under test.
func writePipelineFolder(t *testing.T, dir string, manifest *wfdir.Manifest, files map[string]string) string {
	t.Helper()
	if manifest.Kind == "" {
		manifest.Kind = wfdir.KindPipeline
	}
	if err := wfdir.SaveManifest(dir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for path, content := range files {
		if err := wfdir.WriteFile(dir, path, content); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir
}

// writePipelineBaseline records a baseline for a folder: the content hash of
// each file plus which row it describes.
//
// An unset LiveSHA256 defaults to the file's own content hash, which is the
// shape a clone-from-live leaves behind — the ordinary starting point, and the
// one that arms leg (a) of the drift guard. A test that wants the live
// fingerprint to differ (or to be absent) says so explicitly; the default is
// deliberately not "empty", because empty DISARMS the guard and a folder seeded
// that way would pass every drift test by never checking.
func writePipelineBaseline(t *testing.T, root string, key wfdir.InstanceKey, featureID string,
	files map[string]string, tables map[string]wfdir.TableState) {
	t.Helper()
	inst := &wfdir.InstanceState{
		SourceID: featureID,
		Files:    map[string]wfdir.FileState{},
		Tables:   map[string]wfdir.TableState{},
	}
	for path, content := range files {
		inst.Files[path] = wfdir.FileState{SHA256: wfdir.HashString(content)}
	}
	for path, state := range tables {
		if state.LiveSHA256 == "" {
			if content, known := files[path]; known {
				state.LiveSHA256 = wfdir.HashString(content)
			}
		}
		inst.Tables[path] = state
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	state.Set(key, inst)
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatalf("save state: %v", err)
	}
}

// pipelineManifestOf reads a folder's manifest back for assertions.
func pipelineManifestOf(t *testing.T, root string) *wfdir.Manifest {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.PipelineKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m
}

// pipelineBindingOf reads a folder's binding for one instance.
func pipelineBindingOf(t *testing.T, root string, key wfdir.InstanceKey) wfdir.Binding {
	t.Helper()
	m := pipelineManifestOf(t, root)
	binding, ok, err := m.Binding(key)
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	if !ok {
		t.Fatalf("no binding for %s", key.URL)
	}
	return binding
}

// bindPipelineTable adds one path→table binding to a folder's manifest, for the
// tests that need a second file already pointing somewhere.
func bindPipelineTable(t *testing.T, root string, key wfdir.InstanceKey, path, tableID string) {
	t.Helper()
	m := pipelineManifestOf(t, root)
	binding, _, err := m.Binding(key)
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	m.SetBinding(key, binding.WithTable(path, tableID))
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// pipelineStateOf reads a folder's baseline back for assertions.
func pipelineStateOf(t *testing.T, root string, key wfdir.InstanceKey) *wfdir.InstanceState {
	t.Helper()
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	inst := state.For(key)
	if inst == nil {
		t.Fatalf("no baseline recorded for %s", key.URL)
	}
	return inst
}

// assertPipelineBaselineMatchesDisk is assertBaselineMatchesDisk for a pipeline
// folder: every path the baseline claims is on disk with that hash, and every
// SYNCABLE file on disk is in the baseline.
//
// The syncable qualifier is the whole difference, and it is the invariant this
// loop is most likely to break: a README in the folder is not a file the
// baseline should mention, and one that appeared there would be reported as a
// remote deletion for ever.
func assertPipelineBaselineMatchesDisk(t *testing.T, root string, key wfdir.InstanceKey) {
	t.Helper()
	inst := pipelineStateOf(t, root, key)
	enumeration, err := enumerateFolder(root, wfdir.PipelineKind)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	for path, want := range inst.Hashes() {
		got, ok := enumeration.Files[path]
		if !ok {
			t.Errorf("baseline claims %s, which is not a syncable file on disk", path)
			continue
		}
		if got != want {
			t.Errorf("%s: baseline hash %s, disk hash %s", path, want, got)
		}
	}
}

// --- the metric authoring routes --------------------------------------------
//
// Two handlers, and between them they model the four server properties the
// metric half of the pipeline loop is built around. A fake that skipped any of
// them would let a CLI that got it wrong pass every test here and fail against a
// real instance:
//
//  1. THE RECIPE IS CANONICALIZED ON THE WAY IN. What the row stores is not the
//     bytes the client sent — a `version` is defaulted in — so a client that
//     hashed its own local file for the compare-and-swap would be refused. The
//     fake defaults the same key for that reason alone.
//  2. THE PRECONDITION IS COMPARED AGAINST WHAT THE ROW STORES, whatever the
//     client believed it was fingerprinting, and its three states answer
//     differently: absent is no check, "" is a 400 rather than a silent
//     no-check, and a digest is compare-and-swap.
//  3. THE RECIPE ROUTE REFUSES A LIVE ROW, for every caller including an admin.
//     That refusal is the whole reason the route is safe to publish, and a fake
//     that accepted one would let a CLI that wrote the live metric's id pass —
//     while in production it would leave a metric serving numbers from a
//     definition nobody vetted.
//  4. CREATING IS ADMIN-ONLY and lands `unvetted`, with every verification
//     column empty. A metric born verified is the hole the create body was
//     narrowed to close.

// AddMetric registers a live metric row, filling in what every command reads.
func (f *fakePipelineInstance) AddMetric(id, name, recipe string) *api.Table {
	f.t.Helper()
	return f.AddTable(&api.Table{
		ID: id, Name: name, Kind: api.TableKindMetric,
		MetricRecipe: json.RawMessage(recipe),
		MetricStatus: api.MetricStatusUnvetted,
	})
}

// RecipeOf reads one row's stored recipe back, for assertions.
func (f *fakePipelineInstance) RecipeOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.tables[id]; row != nil {
		return string(row.MetricRecipe)
	}
	return ""
}

// canonicalRecipe is the server's canonicalization, modelled to the one degree
// that matters to a client: the stored bytes are NOT the sent bytes.
//
// `version` is defaulted to 1 when the caller omitted it, which is enough to
// make the point — a client that hashed its own file rather than what the server
// answered would be refused by the precondition, exactly as it would in
// production. Everything else is carried through verbatim, because the recipe
// grammar is the server's and the CLI passes it through untouched.
func canonicalRecipe(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	if _, has := fields["version"]; !has {
		fields["version"] = json.RawMessage("1")
	}
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("canonicalRecipe: %v", err)
	}
	return out
}

// recipeDigest is the server's precondition digest: sha256 over the raw STORED
// bytes, lowercase hex.
func recipeDigest(recipe json.RawMessage) string {
	sum := sha256.Sum256(recipe)
	return hex.EncodeToString(sum[:])
}

// serveCreateMetric models POST /feature/model/metric.
func (f *fakePipelineInstance) serveCreateMetric(w http.ResponseWriter, r *http.Request) {
	// Answered first, and by CODE rather than by prose: the CLI's whole
	// adopt-by-name recovery branches on it.
	if f.nameTakenOnMetricCreate != "" {
		http.Error(w, `{"error":`+strconv.Quote(f.nameTakenOnMetricCreate)+`,"code":"name_taken"}`, http.StatusConflict)
		return
	}
	if f.failMetricCreate != 0 {
		http.Error(w, `{"error":"admin required to create a metric"}`, f.failMetricCreate)
		return
	}
	var in api.CreateMetricInput
	decodeBody(f.t, r, &in)
	f.metricsCreated = append(f.metricsCreated, in)
	f.nextID++
	row := f.AddTable(&api.Table{
		ID:                fmt.Sprintf("table-metric-%d", f.nextID),
		Name:              in.Name,
		Kind:              api.TableKindMetric,
		FeatureID:         in.FeatureID,
		Status:            api.TableStatusPending,
		Description:       in.Description,
		DescriptionSource: api.DescriptionSourceUser,
		MetricRecipe:      canonicalRecipe(f.t, in.Recipe),
		// UNVETTED, forced, whatever the body said — and the body has no field
		// for it, which is the other half of the same guarantee.
		MetricStatus: api.MetricStatusUnvetted,
	})
	f.synced[row.ID] = false
	writeJSON(w, map[string]any{"metric": row})
}

// serveWriteRecipe models PUT /feature/model/:id/metric/recipe.
func (f *fakePipelineInstance) serveWriteRecipe(w http.ResponseWriter, r *http.Request, draftID string) {
	if status := f.failRecipePut[draftID]; status != 0 {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}
	row := f.tables[draftID]
	if row == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if row.Kind != api.TableKindMetric {
		http.Error(w, `{"error":"this is not a metric"}`, http.StatusBadRequest)
		return
	}
	// DRAFTS ONLY. See the header above: a recipe written onto a live row would
	// serve numbers from the new definition while the verification fingerprint
	// sat still.
	if row.ShadowStatus != api.ShadowStatusDraft {
		http.Error(w, `{"error":"a metric's recipe is written on a draft — check one out first"}`, http.StatusBadRequest)
		return
	}
	var in recipeWriteBody
	decodeBody(f.t, r, &in)
	f.recipeWrites = append(f.recipeWrites, recordedRecipeWrite{
		ID: draftID, Recipe: string(in.Recipe),
		ReportingTimezone: in.ReportingTimezone, BaseRecipeSha256: in.BaseRecipeSha256,
	})
	if in.BaseRecipeSha256 != nil {
		if *in.BaseRecipeSha256 == "" {
			http.Error(w, `{"error":"baseRecipeSha256 must be a 64-character lowercase hex sha256"}`, http.StatusBadRequest)
			return
		}
		if recipeDigest(row.MetricRecipe) != *in.BaseRecipeSha256 {
			http.Error(w, `{"error":"this draft's recipe changed since you last read it"}`, http.StatusConflict)
			return
		}
	}
	// An ABSENT recipe keeps whatever the draft holds, which is what makes a
	// timezone-only re-declaration expressible.
	if len(in.Recipe) > 0 {
		row.MetricRecipe = canonicalRecipe(f.t, in.Recipe)
	}
	row.UpdatedAt = row.UpdatedAt.Add(time.Minute)
	writeJSON(w, map[string]any{
		"draftID": draftID, "metricID": row.ParentModelID,
		"recipe": row.MetricRecipe, "additive": false,
		"dimensions": []string{}, "timeColumn": "created_at",
		"inputModels": []string{}, "sourceClosure": []string{},
		"reportingTimezone": "",
	})
}
