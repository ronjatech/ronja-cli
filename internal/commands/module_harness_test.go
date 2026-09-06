package commands

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The MODULE arm of the fake instance.
//
// Additive by construction: fakeInstance already models the /me lookup every
// folder command binds by, and the module loop shares it. What is modelled here
// is only what the module verbs touch — create, the file list/PUT/DELETE, draft
// resolution, checkout, commit (both 409 modes), versions, and the metadata PUT
// that takes the LIVE row only.
//
// Four rules are MODELLED rather than stubbed, because they are the four the
// commands make decisions on and a fake that waved them through would let a
// broken command pass:
//
//   - file writes are refused on a live row (checkout first);
//   - a checkout copies the parent's NAME and TITLE onto the draft, so a
//     metadata patch on the live row leaves the draft's own copy behind — which
//     is exactly the state the push's metadata drift guard has to read correctly;
//   - PUT /module/:id refuses a draft id, because a module's name is claimed
//     against the live-uniqueness index and a draft has no claim to make;
//   - a PARENTLESS commit mints no version row (HeadVersionID is empty) and can
//     still 409, on that same live-name index rather than on the base CAS.
//
// ⚠️ GET /module/:id resolves LIVE rows plus the CALLER'S OWN draft or proposal
// — rmodule's AccessibleForAgentGlobal — and 404s an in-flight row belonging to
// anybody else. Modelled honestly in serveModuleRow, because the own-draft leg
// is precisely what makes the loop complete for a non-admin: without it an
// unpublished module 404s for the person who just created it, and `module
// publish` cannot resolve the row it is meant to publish.
type moduleFake struct {
	rows     map[string]*api.Module
	files    map[string][]api.ModuleFile
	draftOf  map[string]string       // live module id -> its ONE open draft
	versions map[string][]api.Module // parent id -> committed versions, NEWEST FIRST

	// created records every POST /module body.
	created []api.CreateModuleInput
	// committed records the draft id of every commit that LANDED.
	committed []string
	// commitConfirmations records the confirmHeadVersionID of every commit
	// ATTEMPT, "" for one that sent none. Its LENGTH is what pins "no retry
	// loop", and on a first publish "no --overwrite-remote leg either".
	commitConfirmations []string
	// patches records every metadata PUT that landed, in order. A LIST rather
	// than a map keyed by id: "the rename landed once" is a statement about how
	// many requests were made, which a map would collapse.
	patches []recordedModulePatch
	// draftPatches records every NON-EMPTY metadata patch a checkout applied to
	// a draft, in order. Its own list rather than a flag on `patches`: the two
	// answer different questions — `patches` is what the live row was told, and
	// this is what the row the COMMIT publishes was told. A push has to do both,
	// and each exactly once.
	draftPatches []recordedModulePatch

	// nameTaken models the live-uniqueness index on the package name: a
	// PARENTLESS commit of a name in here is refused with the server's 409.
	nameTaken map[string]bool
	// commitHeadVersionID models the base CAS for a PARENT-ATTACHED commit —
	// rmodule.checkCommitBaseCAS. "" switches it off.
	commitHeadVersionID string
	// dependentCount is the blast radius a commit reports.
	dependentCount int
	// sharedFeature makes a create land as a PROPOSAL in an organization-scoped
	// feature. A private create lands LIVE — see serveModule's create branch.
	sharedFeature bool

	next int
}

// recordedModulePatch is one metadata PUT the fake applied.
type recordedModulePatch struct {
	ID    string
	Name  string
	Title string
}

// fakeModuleInitSeed mirrors the `__init__.py` rmodule seeds inside the create
// transaction. A brand-new module is therefore NOT empty, which is the whole
// reason the first push reads the row's files before writing.
const fakeModuleInitSeed = "\"\"\"Shared Ronja module.\"\"\"\n"

func newModuleFake() *moduleFake {
	return &moduleFake{
		rows:      map[string]*api.Module{},
		files:     map[string][]api.ModuleFile{},
		draftOf:   map[string]string{},
		versions:  map[string][]api.Module{},
		nameTaken: map[string]bool{},
	}
}

// AddModule registers a module row and its files, filling in what every command
// reads so a test only states what it is about.
func (f *fakeInstance) AddModule(m *api.Module, files ...api.ModuleFile) *api.Module {
	f.t.Helper()
	if m.FeatureID == "" {
		m.FeatureID = "collection-1"
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	}
	for i := range files {
		files[i].ModuleID = m.ID
		if files[i].UpdatedAt.IsZero() {
			files[i].UpdatedAt = m.UpdatedAt
		}
	}
	f.mod.rows[m.ID] = m
	f.mod.files[m.ID] = files
	return m
}

// ModuleFileContents flattens one module row's files for assertions.
func (f *fakeInstance) ModuleFileContents(id string) map[string]string {
	out := map[string]string{}
	for _, file := range f.mod.files[id] {
		out[file.Path] = file.Content
	}
	return out
}

// serveModule answers everything under /api/v2/module, reporting whether it did.
func (f *fakeInstance) serveModule(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v2/module")
	if !ok {
		return false
	}
	rest = strings.Trim(rest, "/")
	m := f.mod

	// POST /module — create.
	//
	// ⚠️ THE LIFECYCLE HERE IS THE SERVER'S, and getting it wrong is what hid a
	// real bug. rmodule.Add stamps the drafter fields only on the SHARED branch,
	// so the lifecycle trigger classifies a private-feature module as **live**
	// and a shared one as **proposed**. A fake that answered `draft` for a
	// private create let a push write files straight into the created row —
	// which the real server refuses, because file mutations are draft/proposed
	// only.
	if r.Method == "POST" && rest == "" {
		var in api.CreateModuleInput
		decodeBody(f.t, r, &in)
		m.created = append(m.created, in)
		// The live-name uniqueness index fires at INSERT for a private create,
		// since the row is born live. Modelled here as well as at commit: a
		// private folder learns its name is taken at the FIRST PUSH.
		if m.nameTaken[in.Name] && !m.sharedFeature {
			http.Error(w, `{"error":"a live module named \"`+in.Name+`\" already exists"}`, http.StatusConflict)
			return true
		}
		m.next++
		row := &api.Module{
			ID: fmt.Sprintf("module-new-%d", m.next), Lifecycle: api.LifecycleLive,
			Name: in.Name, Title: in.Title, FeatureID: in.FeatureID,
		}
		if m.sharedFeature {
			// A proposal, and its feature is shared — which is what routes a
			// non-admin's `module publish` to the review request.
			row.Lifecycle, row.DrafterUserID = api.LifecycleProposed, fakeCallerUserID
			row.FeatureScope = scopeOrganization
		}
		writeJSON(w, f.AddModule(row, api.ModuleFile{Path: moduleInitFile, Content: fakeModuleInitSeed}))
		return true
	}

	// POST /module/draft/:id/request-review — the governance-shaped path, which
	// carries a literal segment where an id normally goes, so it is matched
	// before the id split below.
	if inner, isDraftRoute := strings.CutPrefix(rest, "draft/"); isDraftRoute {
		id, action, _ := strings.Cut(inner, "/")
		if r.Method == "POST" && action == "request-review" {
			// The server's own first refusal, mirrored: governance.RequestReview
			// is a DRAFT transition and answers "row is not a draft" for
			// anything else. Without this the fake accepted a review request for
			// a proposal — a row that is already in front of the admins — and a
			// test could pass on a call the real server rejects with a 400.
			if row := m.rows[id]; row == nil || row.Lifecycle != api.LifecycleDraft {
				http.Error(w, `{"error":"row is not a draft"}`, http.StatusBadRequest)
				return true
			}
			f.reviewRequested = append(f.reviewRequested, id)
			writeJSON(w, nil)
			return true
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return true
	}

	id, action, _ := strings.Cut(rest, "/")
	row := m.rows[id]
	if row == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return true
	}

	if filePath, isFile := strings.CutPrefix(action, "files/"); isFile {
		return f.serveModuleFile(w, r, row, filePath)
	}

	switch {
	case r.Method == "GET" && action == "":
		f.serveModuleRow(w, row)
	case r.Method == "PUT" && action == "":
		f.serveModulePatch(w, r, row)
	case r.Method == "GET" && action == "files":
		files := m.files[id]
		if files == nil {
			files = []api.ModuleFile{}
		}
		writeJSON(w, files)
	case r.Method == "GET" && action == "draft":
		// A LIST route, unlike the workflow's nullable single row: `GET
		// :id/draft` is rmodule.ListDrafts, and there is exactly one draft.
		out := []api.Module{}
		if draftID, open := m.draftOf[id]; open {
			out = append(out, *m.rows[draftID])
		}
		writeJSON(w, out)
	case r.Method == "GET" && action == "versions":
		rows := m.versions[id]
		if rows == nil {
			rows = []api.Module{}
		}
		writeJSON(w, rows)
	case r.Method == "POST" && action == "checkout":
		f.serveModuleCheckout(w, r, row)
	case r.Method == "POST" && action == "commit":
		f.serveModuleCommit(w, r, row)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
	return true
}

// fakeCallerUserID is the user every fake instance reports from /me, and
// therefore the drafter of every draft these tests create. Spelled once so the
// own-draft leg below is asserting against the same identity the harness serves.
const fakeCallerUserID = "usr-1"

// serveModuleRow answers GET /module/:id the way rmodule.GetForAgent does. Three
// modes, and the middle one is the whole reason this loop works for an ordinary
// user:
//
//   - admin: anything, no user filter;
//   - anybody with reach: LIVE rows;
//   - the DRAFTER: their own draft or proposal too — rmodule's
//     AccessibleForAgentGlobal. A brand-new module is a parentless draft or a
//     proposal, so without this leg its own creator could not read it back and
//     the loop stalled between the first push and the first publish.
//
// A row in flight for SOMEBODY ELSE is 404, never 403: the no-enumeration
// property is what stops a peer learning that a colleague has an unsubmitted
// draft, and modelling it as a 403 here would let a CLI change that leaked it
// pass.
func (f *fakeInstance) serveModuleRow(w http.ResponseWriter, row *api.Module) {
	admin := f.privilegeLevel <= 10
	ownInFlight := row.DrafterUserID == fakeCallerUserID
	if row.Lifecycle != api.LifecycleLive && !admin && !ownInFlight {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, row)
}

// serveModulePatch is PUT /module/:id — the metadata patch, LIVE rows only.
func (f *fakeInstance) serveModulePatch(w http.ResponseWriter, r *http.Request, row *api.Module) {
	var patch api.ModulePatch
	decodeBody(f.t, r, &patch)
	if row.Lifecycle != api.LifecycleLive {
		// Mirrors the route's own rule: a draft id is refused, since the name is
		// claimed against the live-uniqueness index.
		http.Error(w, `{"error":"the id must name the live module"}`, http.StatusBadRequest)
		return
	}
	if patch.Name != "" {
		row.Name = patch.Name
	}
	if patch.Title != "" {
		row.Title = patch.Title
	}
	f.mod.patches = append(f.mod.patches, recordedModulePatch{ID: row.ID, Name: patch.Name, Title: patch.Title})
	writeJSON(w, row)
}

// serveModuleFile is PUT/DELETE :id/files/*path.
func (f *fakeInstance) serveModuleFile(w http.ResponseWriter, r *http.Request, row *api.Module, path string) bool {
	if r.Method != "PUT" && r.Method != "DELETE" {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return true
	}
	// Mirrors rmodule.requireDraft: live canonicals and version snapshots are
	// immutable, so a push that forgot to check out is refused rather than
	// silently editing what consumers pin.
	if row.Lifecycle != api.LifecycleDraft && row.Lifecycle != api.LifecycleProposed {
		http.Error(w, `{"error":"file mutations are only allowed on draft or proposed modules"}`,
			http.StatusBadRequest)
		return true
	}
	contents := f.ModuleFileContents(row.ID)

	switch r.Method {
	case "PUT":
		var in struct {
			Content    string  `json:"content"`
			BaseSha256 *string `json:"baseSha256"`
		}
		decodeBody(f.t, r, &in)
		f.fileWrites = append(f.fileWrites, recordedFileWrite{Method: "PUT", Path: path, BaseSha256: in.BaseSha256})
		if f.refusePrecondition(w, "module", contents, path, in.BaseSha256) {
			return true
		}
		f.putModuleFile(row, path, in.Content)
		writeJSON(w, api.ModuleFile{ID: "file-" + path, ModuleID: row.ID, Path: path, Content: in.Content})
	case "DELETE":
		var in struct {
			BaseSha256 *string `json:"baseSha256"`
		}
		decodeOptionalBody(f.t, r, &in)
		f.fileWrites = append(f.fileWrites, recordedFileWrite{Method: "DELETE", Path: path, BaseSha256: in.BaseSha256})
		// ORDER MATTERS, and it is the server's: the __init__.py refusal is a
		// 400 whatever precondition the request carried, exactly as the workflow
		// entrypoint's is. It is also why `module push` refuses a folder missing
		// that file BEFORE it writes anything.
		if path == moduleInitFile {
			http.Error(w, `{"error":"__init__.py cannot be deleted — it is what makes the module an importable package; empty it instead"}`,
				http.StatusBadRequest)
			return true
		}
		if f.refusePrecondition(w, "module", contents, path, in.BaseSha256) {
			return true
		}
		f.deleteModuleFile(row.ID, path)
		writeJSON(w, map[string]any{})
	}
	return true
}

// serveModuleCheckout is POST :id/checkout — get-or-create the ONE open draft,
// then apply the rmodule.DraftEditInput body to it.
//
// The BODY is modelled, not ignored, because it is the module loop's only way to
// reach a draft's metadata: PUT /module/:id refuses a draft id, and the commit
// overwrites the parent with the DRAFT's name and title. A fake that dropped the
// patch would let a push that never reconciled the draft pass, and the revert
// would only show up at the next publish.
func (f *fakeInstance) serveModuleCheckout(w http.ResponseWriter, r *http.Request, row *api.Module) {
	m := f.mod
	var patch api.ModulePatch
	decodeOptionalBody(f.t, r, &patch)
	if row.Lifecycle != api.LifecycleLive {
		http.Error(w, `{"error":"only a live module can be checked out"}`, http.StatusBadRequest)
		return
	}
	draft, open := m.rows[m.draftOf[row.ID]], false
	if draftID, isOpen := m.draftOf[row.ID]; isOpen {
		// Idempotent, like the real route: a second checkout returns the same
		// draft rather than minting a second one.
		draft, open = m.rows[draftID], true
	}
	if !open {
		m.next++
		files := append([]api.ModuleFile(nil), m.files[row.ID]...)
		draft = f.AddModule(&api.Module{
			ID: fmt.Sprintf("module-draft-%d", m.next), ParentModuleID: row.ID,
			Lifecycle: api.LifecycleDraft, FeatureID: row.FeatureID,
			// The parent's metadata rides along — rmodule seeds the draft from
			// the live definition — which is what leaves a draft holding the
			// PRE-patch name and title after a metadata PUT on the live row.
			Name: row.Name, Title: row.Title,
			DrafterUserID: fakeCallerUserID,
			BaseVersionID: f.moduleHeadVersionID(row.ID),
			UpdatedAt:     row.UpdatedAt.Add(time.Hour),
		}, files...)
		m.draftOf[row.ID] = draft.ID
	}
	// The patch lands on the draft whether it was just seeded or reused —
	// rmodule.DraftEdit runs draftPatchSet on either. Only a patch that CHANGES
	// something is recorded: an empty body is the checkout that resolves a row,
	// and counting it would make "the draft reconcile ran once" unassertable.
	if !patch.Empty() {
		m.draftPatches = append(m.draftPatches, recordedModulePatch{ID: draft.ID, Name: patch.Name, Title: patch.Title})
		applyModulePatch(draft, patch)
		draft.UpdatedAt = draft.UpdatedAt.Add(time.Minute)
	}
	writeJSON(w, draft)
}

// serveModuleCommit is POST :id/commit, with both refusals the CLI has to
// survive: the parentless live-name collision and the parent-attached base CAS.
func (f *fakeInstance) serveModuleCommit(w http.ResponseWriter, r *http.Request, draft *api.Module) {
	m := f.mod
	var in api.ModuleCommitInput
	decodeOptionalBody(f.t, r, &in)
	m.commitConfirmations = append(m.commitConfirmations, in.ConfirmHeadVersionID)

	if draft.Lifecycle != api.LifecycleDraft && draft.Lifecycle != api.LifecycleProposed {
		http.Error(w, `{"error":"row is not a draft"}`, http.StatusBadRequest)
		return
	}

	if draft.ParentModuleID == "" {
		// The first publish of a brand-new module: it becomes live IN PLACE.
		// The only thing that can refuse it is the live-name uniqueness index,
		// and it refuses with a 409 — a conflict with NO parent behind it and
		// nothing an override could overwrite.
		if m.nameTaken[draft.Name] {
			http.Error(w, `{"error":"a live module named \"`+draft.Name+`\" already exists"}`,
				http.StatusConflict)
			return
		}
		draft.Lifecycle = api.LifecycleLive
		draft.DrafterUserID = ""
		m.committed = append(m.committed, draft.ID)
		// HeadVersionID is EMPTY here, and that is the server's behaviour rather
		// than a shortcut: a parentless commit mints no version row, so the
		// module is live with no committed version behind it.
		writeJSON(w, api.ModuleCommitOutcome{
			ModuleID: draft.ID, ParentAttached: false,
			DependentWorkflowCount: m.dependentCount,
		})
		return
	}

	parent := m.rows[draft.ParentModuleID]
	if parent == nil {
		http.Error(w, `{"error":"parent module is no longer live"}`, http.StatusBadRequest)
		return
	}
	// Mirrors rmodule.checkCommitBaseCAS: the confirmation must equal the
	// CURRENT head, not merely be non-empty.
	if m.commitHeadVersionID != "" && in.ConfirmHeadVersionID != m.commitHeadVersionID {
		http.Error(w, `{"error":"this module changed since your draft was created — it is now at version `+
			m.commitHeadVersionID+`"}`, http.StatusConflict)
		return
	}
	// The parent takes the DRAFT's definition, files and metadata alike, and the
	// draft flips to an immutable version snapshot.
	parent.Name, parent.Title = draft.Name, draft.Title
	m.files[parent.ID] = m.files[draft.ID]
	draft.Lifecycle = api.LifecycleVersion
	m.versions[parent.ID] = append([]api.Module{*draft}, m.versions[parent.ID]...)
	delete(m.draftOf, parent.ID)
	m.committed = append(m.committed, draft.ID)
	writeJSON(w, api.ModuleCommitOutcome{
		ModuleID: parent.ID, ParentAttached: true, HeadVersionID: draft.ID,
		DependentWorkflowCount: m.dependentCount,
	})
}

// moduleHeadVersionID is the fake's own head resolution, mirroring rmodule's:
// the newest committed version, or the module's own id when it has none.
func (f *fakeInstance) moduleHeadVersionID(id string) string {
	if versions := f.mod.versions[id]; len(versions) > 0 {
		return versions[0].ID
	}
	return id
}

// putModuleFile writes one file and moves the ROW's updatedAt with it, the way
// a file save does server-side.
func (f *fakeInstance) putModuleFile(row *api.Module, path, content string) {
	row.UpdatedAt = row.UpdatedAt.Add(time.Minute)
	files := f.mod.files[row.ID]
	for i := range files {
		if files[i].Path == path {
			files[i].Content = content
			return
		}
	}
	f.mod.files[row.ID] = append(files, api.ModuleFile{
		ModuleID: row.ID, Path: path, Content: content,
		UpdatedAt: row.UpdatedAt,
	})
}

func (f *fakeInstance) deleteModuleFile(id, path string) {
	kept := f.mod.files[id][:0]
	for _, file := range f.mod.files[id] {
		if file.Path != path {
			kept = append(kept, file)
		}
	}
	f.mod.files[id] = kept
}

// signInAsModuleAdmin points the CLI at the fake with a profile that RECORDS the
// organization — so no /me round trip is needed to bind a folder — as an ADMIN.
//
// Admin is the role a SHARED-feature module needs to be committed at all
// (rmodule's commit is admin-only there), so the sequence tests that end in a
// published module run as one. It is no longer what makes the by-id reads
// resolve: the own-draft leg does that for everybody — see
// signInAsModuleUser, which runs the same loop as a plain user.
func signInAsModuleAdmin(t *testing.T, f *fakeInstance) {
	t.Helper()
	signInAsModulePrincipal(t, f, 10)
}

// signInAsModuleUser is the same sign-in as a NON-ADMIN (privilege counts DOWN,
// so 50 is an ordinary user). It is the principal the module loop actually has
// to work for: a first push creates the module as a parentless draft in a
// private feature, and every read of it afterwards depends on the own-draft leg
// rather than on an admin's unfiltered one.
func signInAsModuleUser(t *testing.T, f *fakeInstance) {
	t.Helper()
	signInAsModulePrincipal(t, f, 50)
}

func signInAsModulePrincipal(t *testing.T, f *fakeInstance, privilegeLevel int) {
	t.Helper()
	f.privilegeLevel = privilegeLevel
	signIn(t, f)
	writeProfile(t, "test", f.URL(), testTenantID, "test-token")
	t.Setenv("RONJA_TOKEN", "")
	t.Setenv("RONJA_URL", "")
}
