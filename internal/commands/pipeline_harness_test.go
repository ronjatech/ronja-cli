package commands

import (
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
	fields map[string][]string
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
	failGet      map[string]int
	failDraftGet map[string]int
	failCheckout map[string]int
	failPut      map[string]int
	failSync     map[string]int
	failCommit   map[string]int
	failReview   map[string]int
	failCreate   int
	failList     int
	failFeature  int
	failQuery    int

	// --- what the fake was asked to do, for assertions -----------------------
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
	// BaseCodeSha256 is the precondition the PUT carried, as a POINTER so the
	// fake can tell ABSENT from PRESENT-AND-EMPTY. The server reads three states
	// and answers 400 to the empty one, so collapsing them here would let a
	// client that dropped `omitempty` — and therefore 400s every unconditional
	// write in production — pass every test.
	BaseCodeSha256 *string
}

// tableUpdateBody is the PUT body as the SERVER sees it, which is not quite
// api.UpdateTableInput: the precondition is a pointer here for the reason above.
type tableUpdateBody struct {
	Code           string   `json:"code"`
	InputModels    []string `json:"inputModels"`
	BaseCodeSha256 *string  `json:"baseCodeSha256"`
}

func newFakePipelineInstance(t *testing.T) *fakePipelineInstance {
	t.Helper()
	f := &fakePipelineInstance{
		t:              t,
		tables:         map[string]*api.Table{},
		draftOf:        map[string]string{},
		buildErr:       map[string]*api.TableBuildError{},
		synced:         map[string]bool{},
		fields:         map[string][]string{},
		rowCounts:      map[string]int64{},
		submitted:      map[string]bool{},
		intervening:    map[string][]api.TableInterveningVersion{},
		features:       map[string]*api.Feature{},
		syncOutcome:    map[string]string{},
		syncMessage:    map[string]string{},
		rejectCommit:   map[string]bool{},
		syncBusy:       map[string]int{},
		failGet:        map[string]int{},
		failDraftGet:   map[string]int{},
		failCheckout:   map[string]int{},
		failPut:        map[string]int{},
		failSync:       map[string]int{},
		failCommit:     map[string]int{},
		failReview:     map[string]int{},
		privilegeLevel: 50,
		queryCSV:       "id,total\n1,42\n2,17\n",
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
		f.updates = append(f.updates, recordedTableUpdate{ID: id, Code: in.Code, InputModels: in.InputModels, BaseCodeSha256: in.BaseCodeSha256})
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
		// Stored VERBATIM on the row that was addressed, which is the property
		// worth modelling: a PUT to the live row is accepted and is silently lost
		// work, so nothing here redirects it to a draft.
		row.Code = in.Code
		row.InputModels = in.InputModels
		row.UpdatedAt = row.UpdatedAt.Add(time.Minute)
		writeJSON(w, nil)

	case action == "" && r.Method == http.MethodGet:
		if status := f.failGet[id]; status != 0 {
			http.Error(w, `{"error":"boom"}`, status)
			return
		}
		row := f.tables[id]
		if row == nil {
			// Production answers 400 {"error":"no rows"} here, NOT 404: a row the
			// caller cannot see is invisible to RLS, so the read finds nothing and
			// cannot tell "not yours" from "not there" — which is the point. This
			// fake said 404 once, and explainUnreadableRefs was written against
			// that fiction: its test passed while the real cross-organization case
			// fell through to the wrong diagnosis. Mirror production here.
			http.Error(w, `{"error":"no rows"}`, http.StatusBadRequest)
			return
		}
		f.writeRow(w, row)
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
		ID:          fmt.Sprintf("table-new-%d", f.nextID),
		Name:        in.Name,
		Kind:        in.Kind,
		FeatureID:   in.FeatureID,
		Code:        in.Code,
		InputModels: in.InputModels,
		Status:      api.TableStatusPending,
	})
	f.synced[row.ID] = false
	f.writeRow(w, row)
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
	draft := f.tables[draftID]
	if draft == nil || draft.ParentModelID == "" {
		http.Error(w, `{"error":"not a draft"}`, http.StatusBadRequest)
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
