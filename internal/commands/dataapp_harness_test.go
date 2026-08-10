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
// harness of its own — GET :id/files silently answers with the caller's draft.
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

	if r.URL.Path == "/api/v2/authentication/me" {
		writeJSON(w, map[string]any{
			"user":   map[string]any{"id": "usr-1", "email": "dev@example.com"},
			"role":   map[string]any{"name": "user", "privilegeLevel": f.privilegeLevel},
			"tenant": map[string]any{"id": testTenantID, "name": "Test Org"},
		})
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
		writeJSON(w, f.apps[draftID])

	case rest == "checkout":
		f.checkouts++
		f.serveCheckout(w, id)

	case rest == "validate":
		f.serveValidate(w, id)

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
		writeJSON(w, app)

	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
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
	writeJSON(w, app)
}

func (f *fakeAppInstance) serveCheckout(w http.ResponseWriter, liveID string) {
	live, ok := f.apps[liveID]
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if draftID, exists := f.draftOf[liveID]; exists {
		// Idempotent, as EnsureDraftForCaller is.
		writeJSON(w, f.apps[draftID])
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
	writeJSON(w, draft)
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
	writeJSON(w, app)
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
	writeJSON(w, app)
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
