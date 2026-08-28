package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The data-app command harness.
//
// A SEPARATE fake from fakeInstance rather than an extension of it. The two
// surfaces look alike from a distance and behave differently in the three places
// these tests are about: a data-app file write reports a compile failure inside
// a 200, a data app has no persist-nothing dry run, and — the one worth a
// harness of its own — a draft is its own row, addressed by its own id.
// Folding that into the workflow fake would have meant a fake that lies about
// one kind or the other.
type fakeAppInstance struct {
	t *testing.T

	apps map[string]*api.DataApp
	// files is keyed by ROW id, so a draft has its own complete set — which is
	// what makes the live-vs-draft divergence expressible.
	files map[string][]api.DataAppFile
	// draftOf maps a live app id to the caller's open draft id.
	draftOf map[string]string
	// checkouts counts POST :id/checkout calls. A first push must not make one —
	// the created row is already the draft — and only a count can prove the
	// absence of a request that would otherwise be invisible in the result.
	checkouts int
	// featureScope answers GET /feature/:id, which is how `app publish` learns
	// whether committing is admin-only.
	featureScope map[string]string
	// privilegeLevel is the signed-in caller's role level (10 = admin, 50 =
	// ordinary user), mirroring sherlock's downward-counting scale.
	privilegeLevel int
	// noFrontendOrigin models an instance with no configured frontend origin —
	// every row comes back WITHOUT a `url`. See the field of the same name on
	// fakeInstance.
	noFrontendOrigin bool

	// kit is the embedded component kit as /docs/api/kit/<path> serves it:
	// kit-relative path to verbatim source. Unauthenticated and outside the
	// /api/v2 surface entirely, which is the point — a kit file is not an app
	// file, so nothing under /dataapp will ever list one.
	kit map[string]string
	// --- the preview harness ------------------------------------------------
	// previewAnswers is the queue POST :id/preview walks, one entry per call;
	// once it runs out the LAST entry repeats. That shape is what makes the two
	// busy cases expressible with the same field: [busy] repeats forever (busy
	// through the retry), [busy, ok] is a queue that clears.
	previewAnswers []fakePreviewAnswer
	previewCalls   int
	// previewRequests and previewTargets record what each call asked for and
	// which row id it was addressed to.
	previewRequests []api.PreviewRequest
	previewTargets  []string

	// blobs backs the two-call frame fetch: GET /feature/file/:id for the row,
	// then GET /file/download/:key for the bytes. Keyed by FILE ID.
	blobs map[string]fakeBlob
	// fileRowCalls counts GET /feature/file/:id — the FALLBACK leg. It is the
	// only way to prove the preferred one-call path was taken: both legs end in
	// the same bytes on disk, so a client that quietly resolved every id would
	// look identical in the result and differ only in the requests it made (and
	// in whether a scoped token could have made them at all).
	fileRowCalls int
	// fileDownloadCalls counts GET /file/download/:key, the leg both paths end on.
	fileDownloadCalls int

	// kitNotFoundBody is what an unknown kit path answers with, modelling the
	// real instance's plain-text /docs/api breadcrumb rather than a JSON error.
	// A test that wants the near-miss case sets it to a body naming a real kit
	// route; the near-miss engine itself lives in the backend's route table and
	// is not worth reimplementing here.
	kitNotFoundBody string

	// --- failure injection -------------------------------------------------
	// compileFails maps a file path to the diagnostics its PUT reports. The
	// write still SUCCEEDS — that is the whole point of the 200 — so this
	// models an author's syntax error, not a rejected request.
	compileFails map[string]string
	// validateFails, when non-empty, makes POST :id/validate answer 400 with it.
	validateFails string
	// failPut / failDelete map a path to a status code, for genuine request
	// failures as opposed to compile failures.
	failPut    map[string]int
	failDelete map[string]int
	// failGetFile maps a path to a status its GET answers with, which is how a
	// RECONCILING read is made to fail: the CLI asks what the server holds after
	// an uncertain write, and a read that fails too leaves the outcome unknown.
	failGetFile map[string]int
	// failPutAfterWrite fails a PUT with the given status AFTER the row has been
	// written, modelling the server's post-commit publish: rdataapp.UpsertFile
	// commits the file and then recompiles out of transaction, so an S3 or
	// throttle failure there is a 500 from a write that already landed.
	//
	// The distinction from failPut is the whole point — one means "the file is
	// not there", the other means "it is".
	failPutAfterWrite map[string]int
	// failDeleteAfterWrite is failPutAfterWrite for a DELETE: the row is gone by
	// the time the status is returned, so the failure comes from a removal that
	// already took effect.
	//
	// It exists because the reconcile's 404 semantics INVERT here — for a PUT a
	// missing file means the write missed, for a DELETE it means the write landed
	// — and failDelete, which fails before the row is touched, can only ever
	// stage the other half.
	failDeleteAfterWrite map[string]int
	// failCommit is the status POST :id/commit answers with; 0 is success. 400
	// is the shape that matters — the server's needs-review rejection.
	failCommit int
	// commitProposes makes POST :id/commit SUCCEED while reporting that nothing
	// went live — the server's answer when a non-admin publishes a brand-new app
	// into a shared feature, which raises a proposal instead. It is a 200, so a
	// client that reads only the status code calls it a publish.
	commitProposes bool
	// failCreate is the status POST /dataapp answers with, carrying
	// failCreateMessage as the `error` field. The two access refusals
	// (`admin required`, `feature not found`) are what push has to translate.
	failCreate        int
	failCreateMessage string

	// --- what the fake was asked to do, for assertions ---------------------
	created   []api.CreateDataAppInput
	patches   []api.DataAppPatch
	committed []string
	discarded []string
	// deletedApps records every DELETE /dataapp/:id — the whole-app removal
	// `discard --delete-app` performs, which must never happen without it.
	deletedApps     []string
	reviewRequested []string
	validated       []string
	// FileReads records the row id every GET :id/files was addressed to.
	//
	// The assertion this exists for: with a draft open, the CLI must never ask
	// for the LIVE app's files. The server would answer with the draft's anyway
	// and say nothing about it, so a caller that did could not tell which row it
	// was looking at — and the baseline it wrote would name the wrong one.
	FileReads []string
	nextID    int

	server *httptest.Server
	// Requests records every request served as "METHOD /path", in order — the
	// cheapest way to assert both that a command avoided a round trip and that
	// it made the ones it did in the right order (push writes the entrypoint
	// last).
	Requests []string
}

func newFakeAppInstance(t *testing.T) *fakeAppInstance {
	t.Helper()
	f := &fakeAppInstance{
		t:              t,
		apps:           map[string]*api.DataApp{},
		files:          map[string][]api.DataAppFile{},
		draftOf:        map[string]string{},
		featureScope:   map[string]string{},
		compileFails:   map[string]string{},
		failPut:        map[string]int{},
		failDelete:     map[string]int{},
		kit:            map[string]string{},
		blobs:          map[string]fakeBlob{},
		privilegeLevel: 50,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAppInstance) URL() string { return f.server.URL }

func (f *fakeAppInstance) Key() wfdir.InstanceKey {
	return wfdir.InstanceKey{URL: f.server.URL, TenantID: testTenantID}
}

// writeRow answers a SINGLE-ROW route the way the real handler does: the stored
// row plus the `url` its view stamps (api/v2/dataapp.DataAppView), on a frontend
// origin that is not the API's — see fakeFrontendOrigin.
//
// Stamped HERE rather than on the stored row, which is what the fake used to do.
// GET /dataapp/query and GET :id/versions answer raw rows and carry no url,
// deliberately — a fake holding the link on the row would hand it to every route
// alike, and the first command to read one of those listings would pass a test
// the real server fails.
func (f *fakeAppInstance) writeRow(w http.ResponseWriter, app *api.DataApp) {
	if app == nil {
		writeJSON(w, nil)
		return
	}
	row := *app
	if !f.noFrontendOrigin {
		row.URL = fakeFrontendOrigin + "/apps/" + row.ID
	}
	writeJSON(w, row)
}

// AddApp registers a live row and its files, filling in the fields every command
// reads so a test only has to state what it is actually about.
func (f *fakeAppInstance) AddApp(app *api.DataApp, files ...api.DataAppFile) *api.DataApp {
	f.t.Helper()
	if app.Name == "" {
		app.Name = "Revenue explorer"
	}
	if app.Entrypoint == "" {
		app.Entrypoint = "App.tsx"
	}
	if app.FeatureID == "" {
		app.FeatureID = "feat-1"
	}
	if app.Lifecycle == "" {
		app.Lifecycle = api.LifecycleLive
	}
	if app.UpdatedAt.IsZero() {
		app.UpdatedAt = time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	}
	if app.BundleFileKey == "" {
		app.BundleFileKey = "dataapp/" + app.ID + ".html"
	}
	app.Normalize()
	for i := range files {
		if files[i].UpdatedAt.IsZero() {
			files[i].UpdatedAt = app.UpdatedAt
		}
		files[i].DataAppID = app.ID
	}
	f.apps[app.ID] = app
	f.files[app.ID] = files
	return app
}

// AddDraft registers a draft of a live app, wired up the way the server does it:
// ParentDataAppID points at the live row, and GET <live>/draft returns it.
func (f *fakeAppInstance) AddDraft(liveID, draftID string, files ...api.DataAppFile) *api.DataApp {
	f.t.Helper()
	live := f.apps[liveID]
	if live == nil {
		f.t.Fatalf("AddDraft: no live data app %s", liveID)
	}
	validated := live.UpdatedAt.Add(time.Hour)
	draft := f.AddApp(&api.DataApp{
		ID:              draftID,
		ParentDataAppID: liveID,
		Lifecycle:       api.LifecycleDraft,
		Name:            live.Name,
		Entrypoint:      live.Entrypoint,
		FeatureID:       live.FeatureID,
		DataAppAccess:   live.DataAppAccess,
		ValidatedAt:     &validated,
		UpdatedAt:       validated,
	}, files...)
	f.draftOf[liveID] = draftID
	return draft
}

func (f *fakeAppInstance) serve(w http.ResponseWriter, r *http.Request) {
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)

	// The read-only component kit on the docs surface. Served BEFORE the auth
	// check below because the real one takes no credential either.
	if kitPath, ok := strings.CutPrefix(r.URL.Path, "/docs/api/kit/"); ok {
		f.serveKitFile(w, kitPath)
		return
	}

	if r.URL.Path == "/api/v2/authentication/me" {
		writeJSON(w, map[string]any{
			"user":   map[string]any{"id": "usr-1", "email": "dev@example.com"},
			"role":   map[string]any{"name": "user", "privilegeLevel": f.privilegeLevel},
			"tenant": map[string]any{"id": testTenantID, "name": "Test Org"},
		})
		return
	}
	// The two legs of a preview frame fetch. BOTH must be matched before the
	// /feature/ branch below, which would otherwise swallow /feature/file/:id and
	// answer a file read with a feature.
	if fileID, ok := strings.CutPrefix(r.URL.Path, "/api/v2/feature/file/"); ok {
		f.serveFileRow(w, fileID)
		return
	}
	if key, ok := strings.CutPrefix(r.URL.Path, "/api/v2/file/download/"); ok {
		// r.URL.Path is DECODED, so the key's own slashes are back — which is
		// also the assertion: the client escaped the whole key into one segment
		// and it round-tripped.
		f.serveFileDownload(w, key)
		return
	}

	// GET /feature/:id — how publish learns whether the feature is shared.
	if featureID, ok := strings.CutPrefix(r.URL.Path, "/api/v2/feature/"); ok {
		scope, known := f.featureScope[featureID]
		if !known {
			scope = "private"
		}
		writeJSON(w, map[string]any{"id": featureID, "name": "Feature", "scope": scope})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v2/dataapp")
	path = strings.TrimPrefix(path, "/")

	// POST /dataapp — create.
	if path == "" && r.Method == http.MethodPost {
		f.serveCreate(w, r)
		return
	}

	id, rest, _ := strings.Cut(path, "/")
	switch {
	case rest == "draft":
		draftID, ok := f.draftOf[id]
		if !ok {
			// The shape that matters: 200 with a null body, not a 404.
			writeJSON(w, nil)
			return
		}
		f.writeRow(w, f.apps[draftID])

	case rest == "checkout":
		f.checkouts++
		f.serveCheckout(w, id)

	case rest == "validate":
		f.serveValidate(w, id)

	case rest == "preview":
		f.servePreview(w, r, id)

	case rest == "commit":
		if f.failCommit != 0 {
			http.Error(w, `{"error":"admin required to commit a shared data-app draft"}`, f.failCommit)
			return
		}
		f.committed = append(f.committed, id)
		if f.commitProposes {
			// Nothing is committed: the row becomes a proposal in place, so the
			// fake deliberately leaves the draft alone.
			writeJSON(w, api.DataAppCommitOutcome{DataAppID: id, FirstPublish: true, Proposed: true})
			return
		}
		f.commitDraft(id)
		writeJSON(w, api.DataAppCommitOutcome{DataAppID: id})

	case rest == "discard":
		f.discarded = append(f.discarded, id)
		f.dropDraft(id)
		writeJSON(w, nil)

	case rest == "request-review":
		f.reviewRequested = append(f.reviewRequested, id)
		writeJSON(w, nil)

	case rest == "files":
		f.serveFileList(w, id)

	case strings.HasPrefix(rest, "files/"):
		f.serveFile(w, r, id, strings.TrimPrefix(rest, "files/"))

	case rest == "" && r.Method == http.MethodPut:
		f.servePatch(w, r, id)

	case rest == "" && r.Method == http.MethodDelete:
		if _, ok := f.apps[id]; !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		f.deletedApps = append(f.deletedApps, id)
		delete(f.apps, id)
		delete(f.files, id)
		writeJSON(w, map[string]any{"dataAppID": id, "markedForDeletion": true})

	case rest == "":
		app, ok := f.apps[id]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		f.writeRow(w, app)

	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// fakePreviewAnswer is one canned answer to POST :id/preview.
//
// Body is sent VERBATIM, which is the point: the CLI writes report.json from
// the bytes rather than from a re-marshal, and only a fake that can send a body
// this package's mirror does not fully describe can prove it.
type fakePreviewAnswer struct {
	// Status is the HTTP status; 0 means 200.
	Status int
	Body   []byte
	// RetryAfter, when set, is sent as the header of the same name.
	RetryAfter string
}

// fakeBlob is one stored file: the row's storage key and its bytes.
type fakeBlob struct {
	Key  string
	Data []byte
}

// AnswerPreview appends one canned preview answer to the queue.
func (f *fakeAppInstance) AnswerPreview(answer fakePreviewAnswer) {
	f.previewAnswers = append(f.previewAnswers, answer)
}

// AnswerPreviewResult is AnswerPreview for an ordinary 200 observation.
func (f *fakeAppInstance) AnswerPreviewResult(result api.PreviewResult) {
	body, err := json.Marshal(result)
	if err != nil {
		f.t.Fatalf("encode preview result: %v", err)
	}
	f.AnswerPreview(fakePreviewAnswer{Body: body})
}

// AnswerPreviewBusy queues the 429 the endpoint answers a spent render pool
// with: a Retry-After header and a body carrying {busy: true}.
func (f *fakeAppInstance) AnswerPreviewBusy(message string) {
	body, err := json.Marshal(map[string]any{
		"busy": true, "message": message, "calibration": "a busy pool says nothing about your app",
	})
	if err != nil {
		f.t.Fatalf("encode busy body: %v", err)
	}
	f.AnswerPreview(fakePreviewAnswer{
		Status: http.StatusTooManyRequests, Body: body, RetryAfter: "15",
	})
}

// AddFileBlob registers a file row and its bytes, reachable by the two-call
// fetch a preview frame needs.
func (f *fakeAppInstance) AddFileBlob(fileID, key string, data []byte) {
	f.blobs[fileID] = fakeBlob{Key: key, Data: data}
}

func (f *fakeAppInstance) servePreview(w http.ResponseWriter, r *http.Request, id string) {
	var in api.PreviewRequest
	decodeBody(f.t, r, &in)
	f.previewRequests = append(f.previewRequests, in)
	f.previewTargets = append(f.previewTargets, id)

	answer := fakePreviewAnswer{}
	if n := len(f.previewAnswers); n > 0 {
		idx := f.previewCalls
		if idx >= n {
			idx = n - 1
		}
		answer = f.previewAnswers[idx]
	}
	f.previewCalls++

	if answer.RetryAfter != "" {
		w.Header().Set("Retry-After", answer.RetryAfter)
	}
	status := answer.Status
	if status == 0 {
		status = http.StatusOK
	}
	body := answer.Body
	if body == nil {
		// The degenerate honest answer: a render that painted and saw nothing.
		body, _ = json.Marshal(api.PreviewResult{
			Rendered: true, Viewport: "desktop", DataAppID: id,
			Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
			ConsoleErrors: []api.PreviewConsoleError{},
			Screenshots:   []api.PreviewScreenshot{},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (f *fakeAppInstance) serveFileRow(w http.ResponseWriter, fileID string) {
	f.fileRowCalls++
	blob, ok := f.blobs[fileID]
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, api.File{
		ID: fileID, Name: "preview.png", FileKey: blob.Key,
		ContentType: "image/png", ContentSize: int64(len(blob.Data)),
	})
}

func (f *fakeAppInstance) serveFileDownload(w http.ResponseWriter, key string) {
	f.fileDownloadCalls++
	for _, blob := range f.blobs {
		if blob.Key == key {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(blob.Data)
			return
		}
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

// AddKitFile registers one component-kit file at its import path.
func (f *fakeAppInstance) AddKitFile(path, source string) {
	f.kit[path] = source
}

// serveKitFile mirrors the docs route: verbatim source as text/plain, and a
// PLAIN-TEXT 404 breadcrumb for anything else — no route is mounted for a path
// that is not in the kit, so an unknown one falls through to the API's own
// not-found page rather than a JSON error envelope.
func (f *fakeAppInstance) serveKitFile(w http.ResponseWriter, path string) {
	source, ok := f.kit[path]
	if !ok {
		body := f.kitNotFoundBody
		if body == "" {
			body = "404 page not found\n\nThis is the Ronja API. Start at /llms.txt\n"
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(source))
}

func (f *fakeAppInstance) serveCreate(w http.ResponseWriter, r *http.Request) {
	if f.failCreate != 0 {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, f.failCreateMessage), f.failCreate)
		return
	}
	var in api.CreateDataAppInput
	decodeBody(f.t, r, &in)
	f.created = append(f.created, in)
	f.nextID++
	// Created as a PARENTLESS DRAFT and empty, exactly as rdataapp.Create does
	// since migration 000495 — the same shape a workflow has always had. The row
	// the create returns IS the row the push writes to; there is no checkout.
	app := f.AddApp(&api.DataApp{
		ID:            fmt.Sprintf("data_app-new-%d", f.nextID),
		Name:          in.Name,
		FeatureID:     in.FeatureID,
		Lifecycle:     api.LifecycleDraft,
		DataAppAccess: in.DataAppAccess,
		// No bundle: nothing has compiled yet.
		BundleFileKey: " ",
	})
	app.BundleFileKey = ""
	f.writeRow(w, app)
}

func (f *fakeAppInstance) serveCheckout(w http.ResponseWriter, liveID string) {
	live, ok := f.apps[liveID]
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if draftID, exists := f.draftOf[liveID]; exists {
		// Idempotent, as EnsureDraftForCaller is.
		f.writeRow(w, f.apps[draftID])
		return
	}
	f.nextID++
	draftID := fmt.Sprintf("data_app-draft-%d", f.nextID)
	copied := append([]api.DataAppFile{}, f.files[liveID]...)
	for i := range copied {
		copied[i].DataAppID = draftID
	}
	draft := f.AddApp(&api.DataApp{
		ID:              draftID,
		ParentDataAppID: liveID,
		Lifecycle:       api.LifecycleDraft,
		Name:            live.Name,
		Entrypoint:      live.Entrypoint,
		FeatureID:       live.FeatureID,
		DataAppAccess:   live.DataAppAccess,
		UpdatedAt:       live.UpdatedAt.Add(time.Minute),
		// buildDraftFromParent leaves bundle_file_key empty by design.
		BundleFileKey: " ",
	}, copied...)
	draft.BundleFileKey = ""
	f.draftOf[liveID] = draftID
	f.writeRow(w, draft)
}

// serveValidate models the FIXED handler: it always recompiles, so a draft whose
// source does not compile is refused however stale its bundle key is.
func (f *fakeAppInstance) serveValidate(w http.ResponseWriter, id string) {
	f.validated = append(f.validated, id)
	app, ok := f.apps[id]
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if f.validateFails != "" {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, f.validateFails), http.StatusBadRequest)
		return
	}
	stamped := app.UpdatedAt.Add(time.Second)
	app.ValidatedAt = &stamped
	app.BundleFileKey = "dataapp/" + id + "-fresh.html"
	f.writeRow(w, app)
}

// serveFileList models the SILENT DRAFT RESOLUTION the real handler performs:
// asked for a live app's files while the caller has a draft open, it answers
// with the DRAFT's, and nothing in the response says so.
//
// Modelled rather than simplified because it is the trap: a CLI that addresses
// the live id gets draft bytes and records them under the live row's identity.
func (f *fakeAppInstance) serveFileList(w http.ResponseWriter, id string) {
	f.FileReads = append(f.FileReads, id)
	if _, ok := f.apps[id]; !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if draftID, hasDraft := f.draftOf[id]; hasDraft {
		id = draftID
	}
	files := f.files[id]
	if files == nil {
		files = []api.DataAppFile{}
	}
	writeJSON(w, files)
}

func (f *fakeAppInstance) serveFile(w http.ResponseWriter, r *http.Request, id, filePath string) {
	switch r.Method {
	case http.MethodGet:
		if status := f.failGetFile[filePath]; status != 0 {
			http.Error(w, `{"error":"boom"}`, status)
			return
		}
		for _, file := range f.files[id] {
			if file.Path == filePath {
				writeJSON(w, file)
				return
			}
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)

	case http.MethodPut:
		if status := f.failPut[filePath]; status != 0 {
			http.Error(w, `{"error":"boom"}`, status)
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		decodeBody(f.t, r, &body)
		target := f.mutationTarget(id)
		saved := f.putFile(target, filePath, body.Content)
		if status := f.failPutAfterWrite[filePath]; status != 0 {
			// The row is already written; only the recompile/upload failed.
			http.Error(w, `{"error":"upload bundle: boom"}`, status)
			return
		}
		resp := map[string]any{
			"id": saved.ID, "dataAppID": target, "path": filePath,
			"content": saved.Content, "updatedAt": saved.UpdatedAt,
		}
		// A failed recompile is reported INSIDE a 200 alongside the saved row:
		// the write committed before the compile ran.
		if message, fails := f.compileFails[filePath]; fails {
			resp["compileError"] = map[string]any{
				"message":     message,
				"diagnostics": []map[string]any{{"message": message, "line": 1, "column": 0, "file": filePath}},
			}
			f.apps[target].ValidatedAt = nil
		}
		writeJSON(w, resp)

	case http.MethodDelete:
		if status := f.failDelete[filePath]; status != 0 {
			http.Error(w, `{"error":"boom"}`, status)
			return
		}
		target := f.mutationTarget(id)
		f.deleteFile(target, filePath)
		if status := f.failDeleteAfterWrite[filePath]; status != 0 {
			// The row is already gone; only the recompile/upload failed.
			http.Error(w, `{"error":"upload bundle: boom"}`, status)
			return
		}
		writeJSON(w, map[string]any{"dataAppID": target})

	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func (f *fakeAppInstance) servePatch(w http.ResponseWriter, r *http.Request, id string) {
	var patch api.DataAppPatch
	decodeBody(f.t, r, &patch)
	f.patches = append(f.patches, patch)
	target := f.mutationTarget(id)
	app := f.apps[target]
	if app == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if patch.Name != nil {
		app.Name = *patch.Name
	}
	if patch.AllowedTableIDs != nil {
		app.AllowedTableIDs = *patch.AllowedTableIDs
	}
	if patch.AllowedSecretIDs != nil {
		app.AllowedSecretIDs = *patch.AllowedSecretIDs
	}
	if patch.AllowedAgentIDs != nil {
		app.AllowedAgentIDs = *patch.AllowedAgentIDs
	}
	if patch.AllowedWorkflowIDs != nil {
		app.AllowedWorkflowIDs = *patch.AllowedWorkflowIDs
	}
	if patch.AllowedCodexIDs != nil {
		app.AllowedCodexIDs = *patch.AllowedCodexIDs
	}
	if patch.AllowedMetricIDs != nil {
		app.AllowedMetricIDs = *patch.AllowedMetricIDs
	}
	if patch.Capabilities != nil {
		app.Capabilities = *patch.Capabilities
	}
	app.Normalize()
	f.writeRow(w, app)
}

// mutationTarget mirrors resolveMutationTarget: a write to a LIVE app lands on
// the caller's draft, forking one when there is none.
func (f *fakeAppInstance) mutationTarget(id string) string {
	app := f.apps[id]
	if app == nil || app.Lifecycle != api.LifecycleLive {
		return id
	}
	if draftID, ok := f.draftOf[id]; ok {
		return draftID
	}
	rec := httptest.NewRecorder()
	f.serveCheckout(rec, id)
	return f.draftOf[id]
}

func (f *fakeAppInstance) putFile(id, path, content string) api.DataAppFile {
	if app := f.apps[id]; app != nil {
		app.UpdatedAt = app.UpdatedAt.Add(time.Minute)
		// Every write clears validated_at; a clean compile re-stamps it.
		stamped := app.UpdatedAt
		app.ValidatedAt = &stamped
	}
	for i := range f.files[id] {
		if f.files[id][i].Path == path {
			f.files[id][i].Content = content
			return f.files[id][i]
		}
	}
	file := api.DataAppFile{
		ID: fmt.Sprintf("dataappfile-%s-%s", id, path), DataAppID: id,
		Path: path, Content: content,
		UpdatedAt: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	}
	f.files[id] = append(f.files[id], file)
	return file
}

func (f *fakeAppInstance) deleteFile(id, path string) {
	kept := f.files[id][:0]
	for _, file := range f.files[id] {
		if file.Path != path {
			kept = append(kept, file)
		}
	}
	f.files[id] = kept
}

// commitDraft moves a draft's content onto its parent and deletes the draft, the
// way commitDraftInTx does.
func (f *fakeAppInstance) commitDraft(draftID string) {
	draft := f.apps[draftID]
	if draft == nil || draft.ParentDataAppID == "" {
		return
	}
	parent := f.apps[draft.ParentDataAppID]
	if parent != nil {
		parent.Name = draft.Name
		parent.DataAppAccess = draft.DataAppAccess
		parent.BundleFileKey = draft.BundleFileKey
		parent.ValidatedAt = draft.ValidatedAt
		parent.UpdatedAt = draft.UpdatedAt.Add(time.Minute)
		copied := append([]api.DataAppFile{}, f.files[draftID]...)
		for i := range copied {
			copied[i].DataAppID = parent.ID
		}
		f.files[parent.ID] = copied
	}
	f.dropDraft(draftID)
}

func (f *fakeAppInstance) dropDraft(draftID string) {
	draft := f.apps[draftID]
	if draft == nil {
		return
	}
	delete(f.draftOf, draft.ParentDataAppID)
	delete(f.apps, draftID)
	delete(f.files, draftID)
}

// FileContents flattens one row's files for assertions.
func (f *fakeAppInstance) FileContents(id string) map[string]string {
	out := map[string]string{}
	for _, file := range f.files[id] {
		out[file.Path] = file.Content
	}
	return out
}

// signInApp points the CLI at a fake data-app instance with a credential in an
// isolated config dir.
func signInApp(t *testing.T, f *fakeAppInstance) {
	t.Helper()
	signInTo(t, f.URL())
}

// writeAppFolder lays out a data-app folder on disk: a manifest of the right
// kind, an optional binding, and the given files.
//
// Returns the root. The baseline is written separately by the tests that need
// one, because "has no baseline" is itself a state under test.
func writeAppFolder(t *testing.T, dir string, manifest *wfdir.Manifest, files map[string]string) string {
	t.Helper()
	if manifest.Kind == "" {
		manifest.Kind = wfdir.KindDataApp
	}
	if manifest.Entrypoint == "" {
		manifest.Entrypoint = wfdir.DataAppKind.DefaultEntrypoint
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

// appManifestOf reads a folder's manifest back for assertions.
func appManifestOf(t *testing.T, root string) *wfdir.Manifest {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.DataAppKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m
}

// appStateOf reads a folder's baseline back for assertions.
func appStateOf(t *testing.T, root string) *wfdir.State {
	t.Helper()
	s, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return s
}

// decodeJSONInto is decodeJSON for a typed payload.
func decodeJSONInto(t *testing.T, out string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), into); err != nil {
		t.Fatalf("output is not JSON (%v):\n%s", err, out)
	}
}
