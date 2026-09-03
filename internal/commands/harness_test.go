package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The command-level test harness.
//
// The wfdir tests cover the folder model in isolation; these cover what the
// COMMANDS do with it — which is where the interesting failures live, because
// they are the layer that decides what to write, what to record as the baseline,
// and what to refuse. A fake instance is the only way to exercise that: the
// bugs worth catching (a baseline claiming a file that is not on disk, a drift
// note overwritten by another) are invisible to a unit test of either half.
//
// Three things every test needs, and they are all here rather than repeated:
// a fake Ronja to answer the three workflow reads, an isolated HOME so nothing
// touches the developer's real credential store, and a way to capture the JSON
// a command prints to os.Stdout.
//
// Kept deliberately small and additive: Phase 3's push/validate/test commands
// need exactly this shape plus more routes on fakeInstance.

// fakeInstance is a Ronja instance serving the workflow read surface.
//
// Rows are keyed by id. Drafts are keyed by the LIVE workflow's id — matching
// GET :id/draft, which answers with the caller's own draft of that workflow —
// and the endpoint answers 200 with a JSON `null` body when there is none,
// because that is what the real one does and the CLI's nil-pointer decode is
// built around it.
type fakeInstance struct {
	t *testing.T

	workflows map[string]*api.Workflow
	files     map[string][]api.WorkflowFile
	// draftOf maps a live workflow id to the id of the caller's open draft.
	draftOf map[string]string

	// Failure injection, so the degradation paths are testable. Each is a
	// status code to answer with instead of the real response; 0 is off.
	failDraft int
	failFiles int
	// failMe answers the organization lookup with a status instead of an
	// identity — a revoked or expired token, or an instance having a bad day.
	// The acting commands must refuse on it and `status` must degrade.
	failMe int
	// noTenant answers the lookup successfully, for a user who belongs to no
	// organization at all. A different state from failMe: the credential works,
	// there is simply nothing to bind a folder to.
	noTenant bool
	// failPut maps a file path to the status a PUT of it should answer with,
	// which is how the mid-push failure case is staged.
	failPut map[string]int
	// failDelete is failPut for DELETE — the way to stage a rejected deletion
	// that is not the entrypoint rule modelled below.
	failDelete map[string]int
	// failPatch is the status PUT :id (the metadata patch) answers with, which
	// stages a push that has written every file and then falls over.
	failPatch int
	// failCommit is the status POST :id/commit answers with; 0 is success.
	// 400 is the shape that matters — it is the server's needs-review
	// rejection, and the one publish falls back to a review request on.
	failCommit int

	// --- optimistic concurrency -------------------------------------------
	// fileWrites records every PUT/DELETE of a file WITH the precondition it
	// carried, which is the only way to tell "sent no precondition" from
	// "sent an empty one" — the difference between "overwrite whatever is
	// there" and "this file must not exist yet".
	fileWrites []recordedFileWrite
	// enforcePreconditions makes the fake behave like the real server: a
	// baseSha256 that does not describe what the row holds is answered with a
	// 409 and the write is not applied. ON by default (newFakeInstance sets
	// it), because a fake that ignored the field would let a CLI sending the
	// WRONG hash pass every test here.
	enforcePreconditions bool
	// commitHeadVersionID models a parent that has been committed to since the
	// draft was seeded: any commit whose confirmHeadVersionID does not equal it
	// is refused with a 409, exactly like rworkflow's checkCommitBaseCAS. ""
	// switches the CAS off.
	commitHeadVersionID string
	// versions is what GET :id/versions answers with, keyed by PARENT id,
	// newest first. Deliberately separate from commitHeadVersionID so a test
	// can stage the two disagreeing.
	versions map[string][]api.Workflow
	// commitConfirmations records the confirmHeadVersionID of every commit
	// attempt, "" for a commit that sent none. Its LENGTH is what pins "no
	// retry loop".
	commitConfirmations []string
	// beforeFileWrite, when set, runs at the top of every file PUT/DELETE.
	//
	// It is the only way to stage the race the preconditions exist for: a
	// change that lands AFTER the push read the file listing and BEFORE its own
	// write. Nothing a test can do from the outside hits that window, because
	// the CLI makes both calls back to back.
	beforeFileWrite func(method, path string)

	// --- runs -------------------------------------------------------------
	// runScript is what GET /workflow/run/:id answers with, in order; the LAST
	// entry repeats forever. That is what makes both terminal cases and the
	// never-terminating one (a script ending on a running entry) expressible
	// without a second knob.
	runScript []api.RunResponse
	runPolls  int
	// runRequests records every POST :id/run as (workflowID, parameterValues,
	// resumeOfRunID), which is how the gate/--write-live refusals are pinned:
	// they must make ZERO of these. The resume field is what pins the other
	// direction — a resume must send a run id and NO parameter values.
	runRequests []recordedRun
	// runHistory is what GET :id/runs answers with, keyed by workflow row id.
	// Deliberately stored in an ARBITRARY order and served that way: the real
	// endpoint applies no default ORDER BY, so a CLI that assumed one would
	// pass here and pick a random run in production.
	runHistory map[string][]api.WorkflowRun
	// runQueries records the raw query string of every GET :id/runs, so the
	// ordering the CLI asks for is asserted rather than assumed.
	runQueries []string
	// failRunsList is the status GET :id/runs answers with; 0 is success.
	failRunsList int
	// failRun is the status POST :id/run answers with, carrying failRunMessage
	// as the server's `error` field — the shape of the credit kill-stop and of
	// the gate refusal the CLI preflights.
	failRun        int
	failRunMessage string
	// failRunBody replaces that bare {"error": …} with a whole JSON object, which
	// is what the concurrency-skip 409 actually looks like: it carries a `code`
	// and names the blocking run in top-level DETAILS, because the message alone
	// cannot be relied on to survive (gt.NewRunInFlightConflict). A CLI that read
	// the run id out of the prose would pass a test that only staged an `error`.
	failRunBody map[string]any
	// failRunGets makes the next N run polls fail with a 500, so the transient-
	// failure tolerance is testable without a flaky network.
	failRunGets int

	// headScript is runScript for GET /workflow/run/:id/head — the lineage-head
	// route `--follow` polls — with the same last-entry-repeats-forever rule.
	//
	// A SEPARATE script rather than a flag on runScript, because the whole point
	// of the route is that the two disagree: the named run sits at `resuming`
	// while the head has moved on to a successor. Each entry carries its OWN id
	// (serveRunPoll's id stamping is skipped here) precisely so a test can stage
	// the id shift that tells a follower a resume happened.
	//
	// An entry whose Status is headRouteMiss is not a response at all: it makes
	// THIS poll answer the route-miss 404, which is how a fleet mid-rollout is
	// staged — an old pod and a new pod answering alternate polls of one follow.
	headScript []api.RunResponse
	headPolls  int
	// headRequests counts every GET of the head route, including the ones
	// answered as a route miss. headPolls only advances through the script, so
	// on an instance with noHeadRoute set it never moves at all — and "did the
	// follow keep TRYING the route" is a question only this counter answers.
	headRequests int
	// noHeadRoute makes the head path answer the way an instance predating it
	// does: the router's plain-text breadcrumb 404, which is NOT JSON. That
	// un-decodable body is the reason the CLI keys its degrade on the status.
	//
	// It is the WHOLE-FLEET version of the same thing headScript's
	// headRouteMiss entry does for ONE poll — see that constant. A fleet is
	// either all old or mid-rollout, and both have to be stageable.
	noHeadRoute bool
	// failHeadGets is the status the head route answers with instead of the
	// script; 0 is off. 400 is the shape that matters — a run id matching
	// nothing, which a new instance answers with and which must NOT be read as
	// an old instance.
	failHeadGets int
	// onPoll, when set, runs after each run poll has been answered.
	//
	// It exists for the Ctrl-C test, which must not raise SIGINT until the
	// command has actually reached the poll loop — and therefore installed its
	// interrupt handler. Reading runPolls from the test goroutine to find that
	// out would be a data race; a hook called from the handler, closing a
	// channel the test waits on, is the same knowledge with a happens-before
	// edge attached.
	onPoll func()

	// tableNames answers GET /feature/model/:id, the best-effort name lookup
	// behind the --write-live refusal. failTableLookup makes it fail instead.
	tableNames      map[string]string
	failTableLookup int
	// agentIDs / secretIDs are the ids GET /agent/:id and GET /secret/:id answer
	// 200 for — the two reference reads `ronja sync check` makes. A SET rather
	// than a row map because nothing reads the payload: the verifier branches on
	// whether the call succeeded and, when it did not, on the status.
	agentIDs  map[string]bool
	secretIDs map[string]bool
	// tableDrafts maps a live table id to the caller's open draft of it. Absent
	// is the ordinary case and answers a JSON `null`, not a 404.
	tableDrafts map[string]string
	// tableCode is the SQL a table row serves. Empty is a real answer (a table
	// whose code the caller cannot see), so it is a separate map from tableNames
	// rather than a field on one.
	tableCode map[string]string
	// featureTables is what GET /feature/model?featureID=… answers with, keyed by
	// feature. A pipeline folder's whole remote leg is gated on this list, so an
	// absent feature answers an EMPTY list rather than a 404 — the real endpoint
	// 404s only for a feature the caller cannot reach, and the CLI reads that as
	// a broken binding.
	featureTables map[string][]*api.TableListItem
	// missStatus is what the three reference reads answer for an id the instance
	// does not hold. 404 by default, and settable to 400 because BOTH are real:
	// a row the caller may not see answers the no-enumeration 404, while a
	// deleted, trashed or cross-tenant row surfaces as table.ErrNoRows — which is
	// rjerr.Input, and therefore 400. A verifier keyed on one of them alone is
	// silently blind to half the cases, so the tests stage both.
	missStatus int

	// validate is what POST /workflow/validate answers with. Nil means clean,
	// which is what most tests want: the validate gate is not the subject.
	validate *api.ValidateResult
	// validated records every validate request body, so a test can assert what
	// the CLI told the server about the candidate — the parameters especially,
	// which the folder does not hold and has to fetch.
	validated []api.ValidateInput
	// saveWarnings maps a file path to the soft warnings its PUT returns.
	saveWarnings map[string][]string
	// privilegeLevel is the signed-in caller's role level (10 = admin, 50 =
	// ordinary user), mirroring sherlock's downward-counting scale.
	privilegeLevel int
	// noFrontendOrigin models an instance with no configured frontend origin:
	// every row comes back WITHOUT a `url`, which is omitempty server-side.
	// This is the normal case on a plain dev box, so the silence it produces is
	// the behaviour worth pinning, not an edge case.
	noFrontendOrigin bool

	// createRuntime is the runtime this instance stamps when a create body names
	// none — the server's own default, which is a real number and not 1 any more.
	// Zero keeps the fake on the older shape, where a create that said nothing
	// produced a row reporting nothing.
	createRuntime int

	// What the fake was asked to do, for assertions.
	created      []api.CreateWorkflowInput
	committed    []string
	publishedIDs []string
	discardedIDs []string
	// deletedWorkflows records every DELETE /workflow/:id — the whole-row
	// removal `discard --delete-workflow` performs, which must never happen
	// without it.
	deletedWorkflows []string
	reviewRequested  []string
	titlePatches     map[string]string
	parameterPatches map[string]*[]api.WorkflowParameter
	// timezonePatches records the declared-zone half of every metadata patch,
	// as a POINTER for the reason parameterPatches is one: "never patched" and
	// "patched to the reset value" are the distinction the three states exist
	// for, and a plain string cannot tell them apart.
	timezonePatches   map[string]*string
	entrypointPatches map[string]string
	// runtimePatches records the runtimeVersion each PUT :id carried. A plain
	// int, unlike the pointer maps above: the field has no reset value and no
	// third state — 0 is "the push did not send one".
	runtimePatches map[string]int
	// tenantZone is what the fake stamps on a create that names no zone,
	// mirroring rworkflow.stampDeclaredZone resolving the organization default.
	tenantZone string
	// zoneUnsupported turns the fake into an instance OLDER than the workflow
	// declared-zone column: `reportingTimezone` is accepted on both the create
	// and the metadata patch, recorded as having arrived, and then DROPPED —
	// which is what gin's binder does with a key no struct field claims, and why
	// the version skew is invisible to the status code. Rows keep the empty
	// declaration such a server would answer with.
	zoneUnsupported bool
	// failRowGetAfterPatch makes GET /workflow/:id fail once a metadata PATCH has
	// been served. It stages the one window a push cannot confirm: the patch was
	// accepted and the read-back that would say what actually landed is the
	// request that fails. Keyed off the patch rather than a request count because
	// the push reads the row several times before it, and a count would break the
	// moment the read order changed.
	failRowGetAfterPatch bool
	patchServed          bool
	nextID               int

	server *httptest.Server
	// Requests records every request served as "METHOD /path", in order — the
	// cheapest way to assert that a command did NOT make a round trip it
	// should have avoided, and that it made the ones it did in the right
	// order (push writes the entrypoint first).
	Requests []string
}

// fakeEntrypointStarter mirrors rworkflow's defaultEntrypointStarter — the
// placeholder the server writes into the entrypoint when a workflow is created.
const fakeEntrypointStarter = "# Write your workflow here.\n"

// recordedRun is one POST :id/run the fake served.
type recordedRun struct {
	WorkflowID      string
	ParameterValues map[string]any
	// ResumeOfRunID is empty for an ordinary run. Recorded because the two
	// halves of a resume's contract are both about this body: it names a run,
	// and it carries no parameter values at all.
	ResumeOfRunID string
}

// recordedFileWrite is one file PUT or DELETE, with the precondition it carried.
// BaseSha256 is nil when the request sent none — which is a different thing from
// an empty string, and the distinction is the whole point of recording it.
type recordedFileWrite struct {
	Method     string
	Path       string
	BaseSha256 *string
}

// writesFor returns the recorded writes for one path, in order.
func (f *fakeInstance) writesFor(path string) []recordedFileWrite {
	var out []recordedFileWrite
	for _, w := range f.fileWrites {
		if w.Path == path {
			out = append(out, w)
		}
	}
	return out
}

func newFakeInstance(t *testing.T) *fakeInstance {
	t.Helper()
	f := &fakeInstance{
		t:                 t,
		workflows:         map[string]*api.Workflow{},
		files:             map[string][]api.WorkflowFile{},
		draftOf:           map[string]string{},
		failPut:           map[string]int{},
		failDelete:        map[string]int{},
		saveWarnings:      map[string][]string{},
		runHistory:        map[string][]api.WorkflowRun{},
		titlePatches:      map[string]string{},
		parameterPatches:  map[string]*[]api.WorkflowParameter{},
		timezonePatches:   map[string]*string{},
		entrypointPatches: map[string]string{},
		runtimePatches:    map[string]int{},
		tenantZone:        "UTC",
		tableNames:        map[string]string{},
		agentIDs:          map[string]bool{},
		secretIDs:         map[string]bool{},
		tableDrafts:       map[string]string{},
		tableCode:         map[string]string{},
		featureTables:     map[string][]*api.TableListItem{},
		missStatus:        http.StatusNotFound,
		privilegeLevel:    50,

		versions:             map[string][]api.Workflow{},
		enforcePreconditions: true,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeInstance) URL() string { return f.server.URL }

// testTenantID is the organization every fake instance reports from /me. Tests
// build binding keys with testKey rather than repeating it.
const testTenantID = "ten-test"

// Key is the (instance, organization) a folder bound against this fake is
// stored under, in both ronja.json and the local baseline.
func (f *fakeInstance) Key() wfdir.InstanceKey {
	return wfdir.InstanceKey{URL: f.server.URL, TenantID: testTenantID}
}

// fakeFrontendOrigin is the origin every fake stamps on a row's `url`, and it
// is DELIBERATELY not the httptest server's.
//
// That is what makes the url assertions mean something. The instance URL a
// profile records is the API origin, and the deploy template puts the backend
// on api.* and the frontend on app.*; a CLI that still templated links from it
// would print 127.0.0.1:<port>/workflows/<id> here. So an assertion that the
// reported link starts with this constant is proof the link came off the
// response rather than being rebuilt locally.
const fakeFrontendOrigin = "https://app.example.test"

// writeRow answers a SINGLE-ROW route the way the real handler does: the stored
// row plus the `url` its view stamps (api/v2/workflow.WorkflowView).
//
// Stamped HERE rather than on the stored row, which is what the fake used to
// do. GET /workflow/query and GET :id/versions answer raw rows and carry no
// url, deliberately — a fake holding the link on the row would hand it to every
// route alike, and the first command to read one of those listings would pass a
// test the real server fails.
func (f *fakeInstance) writeRow(w http.ResponseWriter, wf *api.Workflow) {
	if wf == nil {
		writeJSON(w, nil)
		return
	}
	row := *wf
	if !f.noFrontendOrigin {
		row.URL = fakeFrontendOrigin + "/workflows/" + row.ID
	}
	writeJSON(w, row)
}

// AddWorkflow registers a row and its files, filling in the fields every
// command reads so a test only has to state what it is actually about.
func (f *fakeInstance) AddWorkflow(wf *api.Workflow, files ...api.WorkflowFile) *api.Workflow {
	f.t.Helper()
	if wf.Title == "" {
		wf.Title = "Monthly report"
	}
	if wf.Entrypoint == "" {
		wf.Entrypoint = "main.py"
	}
	if wf.FeatureID == "" {
		wf.FeatureID = "feat-1"
	}
	if wf.UpdatedAt.IsZero() {
		wf.UpdatedAt = time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	}
	for i := range files {
		if files[i].UpdatedAt.IsZero() {
			files[i].UpdatedAt = wf.UpdatedAt
		}
	}
	f.workflows[wf.ID] = wf
	f.files[wf.ID] = files
	return wf
}

// AddDraft registers a draft of a live workflow, wired up the way the server
// does it: ParentWorkflowID points at the live row, and GET <live>/draft
// returns it.
func (f *fakeInstance) AddDraft(liveID, draftID string, files ...api.WorkflowFile) *api.Workflow {
	f.t.Helper()
	live := f.workflows[liveID]
	if live == nil {
		f.t.Fatalf("AddDraft: no live workflow %s", liveID)
	}
	draft := f.AddWorkflow(&api.Workflow{
		ID:               draftID,
		ParentWorkflowID: liveID,
		Lifecycle:        api.LifecycleDraft,
		Title:            live.Title,
		Entrypoint:       live.Entrypoint,
		FeatureID:        live.FeatureID,
		UpdatedAt:        live.UpdatedAt.Add(time.Hour),
	}, files...)
	f.draftOf[liveID] = draftID
	return draft
}

func (f *fakeInstance) serve(w http.ResponseWriter, r *http.Request) {
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)

	if r.URL.Path == "/api/v2/authentication/me" {
		if f.failMe != 0 {
			http.Error(w, `{"error":"no"}`, f.failMe)
			return
		}
		body := map[string]any{
			"user": map[string]any{"id": "usr-1", "email": "dev@example.com"},
			"role": map[string]any{"name": "user", "privilegeLevel": f.privilegeLevel},
			// A workflow folder's binding is keyed by organization, so the wf
			// commands ask for it whenever the credential does not already name
			// one — which is how every test here signs in.
			"tenant": map[string]any{"id": testTenantID, "name": "Test Org"},
		}
		if f.noTenant {
			delete(body, "tenant")
		}
		writeJSON(w, body)
		return
	}
	// The best-effort table-name lookup behind the --write-live refusal. A
	// different route prefix entirely (/feature/model), which is the point: it
	// is a second surface a scope-limited token may not reach, and the CLI has
	// to survive it failing.
	// GET /feature/model?featureID=… — the pipeline loop's first call, and the
	// one that decides whether its whole remote leg runs. Without it every
	// pipeline status test stops at "binding broken: feature … no longer exists",
	// so nothing downstream of it was ever exercised.
	if r.URL.Path == "/api/v2/feature/model" {
		rows := f.featureTables[r.URL.Query().Get("featureID")]
		if rows == nil {
			rows = []*api.TableListItem{}
		}
		writeJSON(w, map[string]any{"token": nil, "total": len(rows), "result": rows})
		return
	}
	if tableID, ok := strings.CutPrefix(r.URL.Path, "/api/v2/feature/model/"); ok {
		// GET :id/draft — the caller's own open draft, answered 200 with a JSON
		// `null` body when there is none, which is what the real route does and
		// what the CLI's nil-pointer decode is built around. Modelled because
		// without it every pipeline drift read fails on the draft lookup and the
		// folder reports `unreadable` — a fake that turns every pipeline test into
		// a test of the same error path.
		if id, isDraft := strings.CutSuffix(tableID, "/draft"); isDraft {
			if draftID, has := f.tableDrafts[id]; has {
				writeJSON(w, map[string]any{"id": draftID, "parentModelID": id})
				return
			}
			writeJSON(w, nil)
			return
		}
		if f.failTableLookup != 0 {
			http.Error(w, `{"error":"no"}`, f.failTableLookup)
			return
		}
		name, known := f.tableNames[tableID]
		if !known {
			http.Error(w, `{"error":"not found"}`, f.missStatus)
			return
		}
		// `code` matters for the pipeline drift legs: it is what the CLI
		// canonicalizes, de-aliases and hashes against the recorded fingerprint.
		// A fake that always served "" made every in-step fixture look drifted
		// against a lock recording real SQL — and made a drifted one look in step
		// against a lock recording "".
		writeJSON(w, map[string]any{"id": tableID, "name": name, "code": f.tableCode[tableID]})
		return
	}
	// The two reference reads `ronja sync check` makes. Neither serves a
	// payload anything branches on — see agentIDs.
	if agentID, ok := strings.CutPrefix(r.URL.Path, "/api/v2/agent/"); ok {
		if !f.agentIDs[agentID] {
			http.Error(w, `{"error":"not found"}`, f.missStatus)
			return
		}
		writeJSON(w, map[string]any{"id": agentID, "name": agentID})
		return
	}
	if secretID, ok := strings.CutPrefix(r.URL.Path, "/api/v2/secret/"); ok {
		if !f.secretIDs[secretID] {
			http.Error(w, `{"error":"not found"}`, f.missStatus)
			return
		}
		writeJSON(w, map[string]any{"id": secretID, "name": secretID})
		return
	}
	if f.serveWrites(w, r) {
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v2/workflow/")

	// GET /workflow/run/:runID — the poll. Checked before the switch below,
	// whose default branch would otherwise read "run/<id>" as a workflow id.
	if runID, ok := strings.CutPrefix(path, "run/"); ok {
		if headOf, isHead := strings.CutSuffix(runID, "/head"); isHead {
			f.serveRunHeadPoll(w, headOf)
			return
		}
		f.serveRunPoll(w, runID)
		return
	}

	switch {
	case strings.HasSuffix(path, "/runs"):
		// GET :id/runs — the run history --resume picks a target out of. A
		// PAGINATED envelope, like the real route, and served in the order the
		// history was staged in: the endpoint has no default ordering, so a CLI
		// that did not ask for one must not be rescued by the fake.
		f.runQueries = append(f.runQueries, r.URL.RawQuery)
		if f.failRunsList != 0 {
			http.Error(w, `{"error":"boom"}`, f.failRunsList)
			return
		}
		runs := f.runHistory[strings.TrimSuffix(path, "/runs")]
		if runs == nil {
			runs = []api.WorkflowRun{}
		}
		writeJSON(w, map[string]any{"token": nil, "total": len(runs), "result": runs})

	case strings.HasSuffix(path, "/draft"):
		id := strings.TrimSuffix(path, "/draft")
		if f.failDraft != 0 {
			http.Error(w, `{"error":"boom"}`, f.failDraft)
			return
		}
		draftID, ok := f.draftOf[id]
		if !ok {
			// The shape that matters: 200 with a null body, not a 404.
			writeJSON(w, nil)
			return
		}
		f.writeRow(w, f.workflows[draftID])

	case strings.Contains(path, "/files/"):
		// GET one file by path. Only the push's timed-out-PUT reconciliation
		// reads a single file — everything else wants the whole set — and 404
		// for a path the row does not hold is the answer it acts on.
		id, filePath, _ := strings.Cut(path, "/files/")
		for _, file := range f.files[id] {
			if file.Path == filePath {
				writeJSON(w, file)
				return
			}
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)

	case strings.HasSuffix(path, "/versions"):
		// A LIST route: bare rows, and deliberately NOT through writeRow, since
		// the real one stamps no `url` on a listing. Newest first, which is the
		// order the CAS anchor is read out of (element 0).
		rows := f.versions[strings.TrimSuffix(path, "/versions")]
		if rows == nil {
			rows = []api.Workflow{}
		}
		writeJSON(w, rows)

	case strings.HasSuffix(path, "/files"):
		id := strings.TrimSuffix(path, "/files")
		if f.failFiles != 0 {
			http.Error(w, `{"error":"boom"}`, f.failFiles)
			return
		}
		if _, ok := f.workflows[id]; !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		files := f.files[id]
		if files == nil {
			files = []api.WorkflowFile{}
		}
		writeJSON(w, files)

	default:
		if f.failRowGetAfterPatch && f.patchServed {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		wf, ok := f.workflows[path]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		f.writeRow(w, wf)
	}
}

// serveWrites handles everything that CHANGES something, plus validate.
// Reports whether it answered, so the read surface above stays as it was.
//
// The lifecycle verbs are modelled rather than stubbed — checkout copies the
// live files into a new draft row, commit moves them onto the parent and
// deletes the draft — because the push/publish/discard tests are about the
// state machine, and a fake that answered 200 to everything would let a
// command that checks out twice, or publishes the wrong row, pass.
func (f *fakeInstance) serveWrites(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/")

	// DELETE /workflow/:id — whole-row soft delete. Matched before the file
	// routes below, which carry a /files/ segment this one never has.
	if r.Method == http.MethodDelete && strings.HasPrefix(path, "workflow/") &&
		!strings.Contains(path, "/files/") {
		id := strings.TrimPrefix(path, "workflow/")
		if _, ok := f.workflows[id]; !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return true
		}
		f.deletedWorkflows = append(f.deletedWorkflows, id)
		delete(f.workflows, id)
		delete(f.files, id)
		writeJSON(w, map[string]any{"workflowID": id, "markedForDeletion": true})
		return true
	}

	// POST /workflow — create.
	if r.Method == "POST" && (path == "workflow" || path == "workflow/") {
		var in api.CreateWorkflowInput
		decodeBody(f.t, r, &in)
		f.created = append(f.created, in)
		f.nextID++
		id := fmt.Sprintf("wf-new-%d", f.nextID)
		entrypoint := in.Entrypoint
		if entrypoint == "" {
			entrypoint = "main.py"
		}
		// Mirrors rworkflow.stampDeclaredZone: a create that names no zone gets
		// the organization default, so a workflow is never born without one.
		zone := in.ReportingTimezone
		if zone == "" {
			zone = f.tenantZone
		}
		if f.zoneUnsupported {
			// An instance without the column has nothing to stamp and nothing to
			// answer with — not even the organization default.
			zone = ""
		}
		// Echoed rather than dropped: the server stamps the runtime it was sent,
		// and falls back to its OWN default when the body named none. A fake that
		// answered 0 either way would let a test asserting on the created ROW
		// pass against a create that never carried it.
		runtimeVersion := in.RuntimeVersion
		if runtimeVersion == 0 {
			runtimeVersion = f.createRuntime
		}
		created := f.AddWorkflow(&api.Workflow{
			ID:                id,
			Lifecycle:         api.LifecycleDraft,
			Title:             in.Title,
			RuntimeVersion:    runtimeVersion,
			Entrypoint:        entrypoint,
			FeatureID:         in.FeatureID,
			Parameters:        in.Parameters,
			ReportingTimezone: zone,
			Hidden:            true,
		}, api.WorkflowFile{
			// Mirrors rworkflow.Add (store.go): the entrypoint file is SEEDED
			// inside the create transaction, because the file-tree panel and
			// the entrypoint integrity check in Commit both rely on there
			// always being a file matching Entrypoint. A brand-new workflow is
			// therefore not empty — which is the whole reason `wf push` cannot
			// assume it is.
			WorkflowID: id, Path: entrypoint, Content: fakeEntrypointStarter,
		})
		f.writeRow(w, created)
		return true
	}
	if r.Method == "POST" && path == "workflow/validate" {
		var in api.ValidateInput
		decodeBody(f.t, r, &in)
		f.validated = append(f.validated, in)
		result := f.validate
		if result == nil {
			result = &api.ValidateResult{Findings: []api.ValidateFinding{}}
		}
		writeJSON(w, result)
		return true
	}
	// POST /workflow/draft/:id/request-review — note the governance prefix.
	if r.Method == "POST" && strings.HasPrefix(path, "workflow/draft/") && strings.HasSuffix(path, "/request-review") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "workflow/draft/"), "/request-review")
		f.reviewRequested = append(f.reviewRequested, id)
		writeJSON(w, nil)
		return true
	}

	rest, ok := strings.CutPrefix(path, "workflow/")
	if !ok {
		return false
	}
	id, action, _ := strings.Cut(rest, "/")

	// File writes: PUT/DELETE workflow/:id/files/*path.
	if filePath, isFile := strings.CutPrefix(action, "files/"); isFile {
		if f.beforeFileWrite != nil && (r.Method == "PUT" || r.Method == "DELETE") {
			f.beforeFileWrite(r.Method, filePath)
		}
		switch r.Method {
		case "PUT":
			var in struct {
				Content    string  `json:"content"`
				BaseSha256 *string `json:"baseSha256"`
			}
			decodeBody(f.t, r, &in)
			f.fileWrites = append(f.fileWrites, recordedFileWrite{Method: "PUT", Path: filePath, BaseSha256: in.BaseSha256})
			if status := f.failPut[filePath]; status != 0 {
				http.Error(w, `{"error":"`+filePath+` is not acceptable"}`, status)
				return true
			}
			// The precondition is checked BEFORE the write and rolls it back by
			// simply not doing it, which is what the server's in-transaction
			// check amounts to from the outside.
			if f.refusePrecondition(w, id, filePath, in.BaseSha256) {
				return true
			}
			f.putFile(id, filePath, in.Content)
			writeJSON(w, map[string]any{
				"id": "file-" + filePath, "workflowID": id, "path": filePath,
				"content": in.Content, "warnings": f.saveWarnings[filePath],
			})
			return true
		case "DELETE":
			// A DELETE legitimately carries NO body at all — that is the shape
			// the route took before preconditions existed, and still the shape
			// of an unconditional delete.
			var in struct {
				BaseSha256 *string `json:"baseSha256"`
			}
			decodeOptionalBody(f.t, r, &in)
			f.fileWrites = append(f.fileWrites, recordedFileWrite{Method: "DELETE", Path: filePath, BaseSha256: in.BaseSha256})
			if status := f.failDelete[filePath]; status != 0 {
				http.Error(w, `{"error":"`+filePath+` cannot be removed"}`, status)
				return true
			}
			// ORDER MATTERS, and it is the server's: rworkflow's
			// DeleteFileChecked refuses the entrypoint BEFORE it looks at the
			// precondition, so an entrypoint delete is a 400 whatever hash it
			// carries. Checking the precondition first here would answer 409 to
			// a request production answers 400 to — and `wf publish`'s
			// needs-review fallback is keyed on exactly that difference, so a
			// fake with the two swapped could hide a real routing bug.
			if wf := f.workflows[id]; wf != nil && wf.Entrypoint == filePath {
				// Mirrors the server: the file the row NAMES as its entrypoint
				// cannot be deleted. Note that it is the row's current
				// entrypoint, not the one it was created with — which is what
				// makes a rename possible at all, and what the push's
				// PUT → patch → DELETE ordering depends on.
				http.Error(w, `{"error":"cannot delete the entrypoint file"}`, http.StatusBadRequest)
				return true
			}
			if f.refusePrecondition(w, id, filePath, in.BaseSha256) {
				return true
			}
			f.deleteFile(id, filePath)
			writeJSON(w, map[string]any{})
			return true
		}
	}

	if r.Method == "PUT" && action == "" {
		var patch api.WorkflowPatch
		decodeBody(f.t, r, &patch)
		if f.failPatch != 0 {
			http.Error(w, `{"error":"boom"}`, f.failPatch)
			return true
		}
		wf := f.workflows[id]
		if wf == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return true
		}
		if patch.Entrypoint != "" && patch.Entrypoint != wf.Entrypoint {
			// Mirrors rworkflow's Update (store.go): an entrypoint the workflow
			// holds no file for is refused, because a run of it would fail
			// immediately. This is the rule the push's ordering exists for, so
			// modelling it is what makes the ordering test mean something.
			if _, ok := f.FileContents(id)[patch.Entrypoint]; !ok {
				http.Error(w, `{"error":"entrypoint file `+patch.Entrypoint+` does not exist in this workflow — upsert it before setting the entrypoint"}`,
					http.StatusBadRequest)
				return true
			}
			wf.Entrypoint = patch.Entrypoint
			f.entrypointPatches[id] = patch.Entrypoint
		}
		if patch.Title != "" {
			wf.Title = patch.Title
			f.titlePatches[id] = patch.Title
		}
		// Mirrors the server's optional.V semantics: an ABSENT key leaves the
		// declaration alone, an explicit list (including []) replaces it. The
		// recorded pointer is what lets a test tell "never patched" from
		// "patched to none" — the distinction the whole feature turns on.
		if patch.Parameters != nil {
			wf.Parameters = *patch.Parameters
			f.parameterPatches[id] = patch.Parameters
		}
		// Mirrors rworkflow.resolveDeclaredZonePatch: absent leaves the
		// declaration alone, and an explicit "" is stored as the LITERAL "UTC"
		// rather than as NULL — which is what keeps an empty column meaning
		// "nobody has ever declared a calendar here" and nothing else.
		if patch.ReportingTimezone != nil {
			zone := *patch.ReportingTimezone
			if zone == "" {
				zone = "UTC"
			}
			// The request ARRIVED either way — that is what the recorded patch
			// means, and an old server is precisely one that receives it and does
			// nothing. Only the stored value is conditional.
			if !f.zoneUnsupported {
				wf.ReportingTimezone = zone
			}
			f.timezonePatches[id] = patch.ReportingTimezone
		}
		// Mirrors rworkflow.CheckRuntimeUpgrade: the runtime moves ONE WAY, so a
		// value below the row's is a 400 rather than a silent no-op. Modelled
		// because the CLI's own refusal runs BEFORE any write, and a test that
		// only exercised the client-side guard would not notice the day the
		// server's disappeared.
		if patch.RuntimeVersion != 0 {
			if patch.RuntimeVersion < wf.RuntimeVersion {
				http.Error(w, `{"error":"runtimeVersion is a one-way upgrade"}`, http.StatusBadRequest)
				return true
			}
			wf.RuntimeVersion = patch.RuntimeVersion
			f.runtimePatches[id] = patch.RuntimeVersion
		}
		f.patchServed = true
		writeJSON(w, nil)
		return true
	}
	if r.Method != "POST" {
		return false
	}

	switch action {
	case "run":
		var in struct {
			ParameterValues map[string]any `json:"parameterValues"`
			ResumeOfRunID   string         `json:"resumeOfRunID"`
		}
		decodeBody(f.t, r, &in)
		f.runRequests = append(f.runRequests, recordedRun{
			WorkflowID: id, ParameterValues: in.ParameterValues, ResumeOfRunID: in.ResumeOfRunID,
		})
		if f.failRun != 0 {
			if f.failRunBody != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.failRun)
				_ = json.NewEncoder(w).Encode(f.failRunBody)
				return true
			}
			http.Error(w, `{"error":"`+f.failRunMessage+`"}`, f.failRun)
			return true
		}
		// Mirrors manalysis.resolveResume's rule 4: a resume inherits the
		// target's parameters, so supplying any is refused rather than merged.
		// Modelled because the CLI's whole job on this path is not to send them.
		if in.ResumeOfRunID != "" && len(in.ParameterValues) > 0 {
			http.Error(w, `{"error":"a resume inherits the original run's parameters — omit parameterValues"}`,
				http.StatusBadRequest)
			return true
		}
		// Mirrors the real endpoint: the run row comes back IMMEDIATELY at
		// status running, and everything else is learned by polling.
		writeJSON(w, api.WorkflowRun{
			ID: "run-1", WorkflowID: id, Status: api.RunStatusRunning,
			ParameterValues: in.ParameterValues,
			ExecutedAt:      time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC),
		})
		return true
	case "checkout":
		if _, exists := f.draftOf[id]; exists {
			f.t.Errorf("checkout called for %s, which already has a draft", id)
		}
		f.nextID++
		draft := f.AddDraft(id, fmt.Sprintf("draft-%d", f.nextID), append([]api.WorkflowFile(nil), f.files[id]...)...)
		// Mirrors rworkflow's buildDraftFromParent (store_draft.go): a checkout
		// copies the parent's PARAMETERS as well as its files. Without this the
		// fake produces a draft declaring none, and the CLI's parameter drift
		// guard fires on a difference the real server never creates.
		if parent := f.workflows[id]; parent != nil {
			draft.Parameters = append([]api.WorkflowParameter(nil), parent.Parameters...)
			// And its declared zone, for the same reason: buildDraftFromParent
			// carries the parent's calendar rather than re-stamping today's
			// organization default, so a checkout must not look like a change.
			draft.ReportingTimezone = parent.ReportingTimezone
			// And its runtime — same rule, and it is what makes the upgrade
			// legible: the draft starts at the parent's generation, the push
			// raises it there, and commit publishes the flip.
			draft.RuntimeVersion = parent.RuntimeVersion
		}
		f.writeRow(w, draft)
		return true
	case "commit":
		// Optional body, like the real route: an ordinary commit sends none.
		var in api.CommitDraftInput
		decodeOptionalBody(f.t, r, &in)
		f.commitConfirmations = append(f.commitConfirmations, in.ConfirmHeadVersionID)
		if f.failCommit != 0 {
			http.Error(w, `{"error":"admin required to commit a shared workflow draft"}`, f.failCommit)
			return true
		}
		// Mirrors rworkflow.checkCommitBaseCAS: the confirmation must equal the
		// CURRENT head, not merely be non-empty.
		if f.commitHeadVersionID != "" && in.ConfirmHeadVersionID != f.commitHeadVersionID {
			http.Error(w, `{"error":"this workflow changed since your draft was created — it is now at version `+
				f.commitHeadVersionID+`. Re-read and re-apply your changes, or commit with confirmHeadVersionID=`+
				f.commitHeadVersionID+` to overwrite those changes"}`, http.StatusConflict)
			return true
		}
		f.committed = append(f.committed, id)
		draft := f.workflows[id]
		if draft != nil && draft.ParentWorkflowID != "" {
			parent := draft.ParentWorkflowID
			f.files[parent] = f.files[id]
			f.workflows[parent].Title = draft.Title
			delete(f.draftOf, parent)
			delete(f.workflows, id)
		}
		writeJSON(w, nil)
		return true
	case "publish":
		f.publishedIDs = append(f.publishedIDs, id)
		if wf := f.workflows[id]; wf != nil {
			wf.Lifecycle = api.LifecycleLive
			wf.Hidden = false
		}
		writeJSON(w, nil)
		return true
	case "discard":
		f.discardedIDs = append(f.discardedIDs, id)
		if draft := f.workflows[id]; draft != nil && draft.ParentWorkflowID != "" {
			delete(f.draftOf, draft.ParentWorkflowID)
		}
		delete(f.workflows, id)
		delete(f.files, id)
		writeJSON(w, nil)
		return true
	}
	return false
}

// serveRunPoll answers one GET /workflow/run/:runID from the script, advancing
// through it and repeating the last entry forever.
//
// Repeating rather than exhausting is what lets one mechanism express both "it
// finishes on the third poll" and "it never finishes" — the latter being the
// only way to test the timeout path.
func (f *fakeInstance) serveRunPoll(w http.ResponseWriter, runID string) {
	if f.onPoll != nil {
		defer f.onPoll()
	}
	if f.failRunGets > 0 {
		f.failRunGets--
		http.Error(w, `{"error":"instance is restarting"}`, http.StatusBadGateway)
		return
	}
	if len(f.runScript) == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	i := f.runPolls
	if i >= len(f.runScript) {
		i = len(f.runScript) - 1
	}
	f.runPolls++
	resp := f.runScript[i]
	resp.ID = runID
	if resp.Steps == nil {
		resp.Steps = []api.StepDTO{}
	}
	writeJSON(w, resp)
}

// headRouteMiss is the headScript entry that is not an answer: staged as a
// response's Status, it makes that ONE poll answer the route-miss 404 an
// instance predating --follow gives. It is not a real status, and is spelled
// with a NUL so it can never collide with one.
//
// A per-poll knob rather than only the whole-fleet noHeadRoute, because Ronja
// runs many instances behind one load balancer: mid-rollout, consecutive polls
// of a single follow are answered by pods that disagree about whether the route
// exists, and that — not the all-old fleet — is where a one-shot old-instance
// decision goes wrong in both directions.
const headRouteMiss = "\x00route-miss"

// writeHeadRouteMiss answers the way the router does for a path it has no route
// for: the plain-text breadcrumb, NOT JSON. Sharing one writer keeps the
// whole-fleet and per-poll knobs answering identically, which is the point of
// staging them at all.
func (f *fakeInstance) writeHeadRouteMiss(w http.ResponseWriter, runID string) {
	http.Error(w, "no route for GET /api/v2/workflow/run/"+runID+"/head\n", http.StatusNotFound)
}

// serveRunHeadPoll answers one GET /workflow/run/:runID/head from headScript,
// advancing through it and repeating the last entry forever.
//
// Three deliberate differences from serveRunPoll. The 404 branch is the ROUTE
// being absent (an instance predating --follow) and answers plain text, not
// JSON, exactly as the router's breadcrumb does — a fake that answered
// `{"error":...}` would let a CLI decoding the body pass. A missing RUN is a
// 400 instead, staged through failHeadGets, because that is what the real route
// does and the two must not be confusable. And the scripted id is left ALONE:
// the head's id is the payload here, not bookkeeping.
func (f *fakeInstance) serveRunHeadPoll(w http.ResponseWriter, runID string) {
	if f.onPoll != nil {
		defer f.onPoll()
	}
	f.headRequests++
	if f.noHeadRoute {
		f.writeHeadRouteMiss(w, runID)
		return
	}
	if f.failHeadGets != 0 {
		// A JSON body with a JSON content type, not http.Error's text/plain: the
		// property under test is that the CLI keys on the STATUS, and staging a
		// body the real route would never send lets a CLI that sniffs bodies pass.
		writeJSONStatus(w, f.failHeadGets, map[string]any{"error": "no rows in result set"})
		return
	}
	if len(f.headScript) == 0 {
		http.Error(w, `{"error":"no rows in result set"}`, http.StatusBadRequest)
		return
	}
	i := f.headPolls
	if i >= len(f.headScript) {
		i = len(f.headScript) - 1
	}
	f.headPolls++
	resp := f.headScript[i]
	if resp.Status == headRouteMiss {
		// This poll landed on an instance that does not have the route. The
		// COUNTER still moved, because the script is a schedule of polls rather
		// than of answers.
		f.writeHeadRouteMiss(w, runID)
		return
	}
	if resp.ID == "" {
		resp.ID = runID
	}
	if resp.Steps == nil {
		resp.Steps = []api.StepDTO{}
	}
	writeJSON(w, resp)
}

// putFile writes one file and moves the ROW's updatedAt with it, the way the
// server does: UpsertFile re-derives the workflow's binding columns in the same
// transaction, so a file write is a row write. That is what makes "which row
// did the baseline's timestamp come from" observable.
func (f *fakeInstance) putFile(id, path, content string) {
	if wf := f.workflows[id]; wf != nil {
		wf.UpdatedAt = wf.UpdatedAt.Add(time.Minute)
	}
	for i := range f.files[id] {
		if f.files[id][i].Path == path {
			f.files[id][i].Content = content
			return
		}
	}
	f.files[id] = append(f.files[id], api.WorkflowFile{
		WorkflowID: id, Path: path, Content: content,
		UpdatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	})
}

func (f *fakeInstance) deleteFile(id, path string) {
	kept := f.files[id][:0]
	for _, file := range f.files[id] {
		if file.Path != path {
			kept = append(kept, file)
		}
	}
	f.files[id] = kept
}

// FileContents flattens one row's files for assertions.
func (f *fakeInstance) FileContents(id string) map[string]string {
	out := map[string]string{}
	for _, file := range f.files[id] {
		out[file.Path] = file.Content
	}
	return out
}

func decodeBody(t *testing.T, r *http.Request, out any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatalf("decode %s %s body: %v", r.Method, r.URL.Path, err)
	}
}

// decodeOptionalBody is decodeBody for the two routes whose body is genuinely
// optional — DELETE :id/files/*path and POST :id/commit. An absent body leaves
// `out` at its zero value, which is exactly what gt does server-side, and is
// what makes "no precondition" and "no override" expressible as sending nothing.
func decodeOptionalBody(t *testing.T, r *http.Request, out any) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", r.Method, r.URL.Path, err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s %s body %q: %v", r.Method, r.URL.Path, body, err)
	}
}

// refusePrecondition mirrors rworkflow.checkFilePrecondition, which is the
// point: a CLI that sends a hash of the wrong thing has to fail here the way it
// would in production, not be waved through by a fake that ignores the field.
//
// Reports whether it answered (with a 409).
func (f *fakeInstance) refusePrecondition(w http.ResponseWriter, id, path string, want *string) bool {
	if want == nil || !f.enforcePreconditions {
		return false
	}
	content, exists := f.FileContents(id)[path]
	switch {
	case *want == "" && !exists:
		return false
	case *want == "":
		http.Error(w, `{"error":"`+path+` already exists on this workflow, but the write asserted it did not"}`,
			http.StatusConflict)
	case exists && wfdir.HashString(content) == *want:
		return false
	case !exists:
		http.Error(w, `{"error":"`+path+` does not exist on this workflow, but the write asserted its content hashed to `+*want+`"}`,
			http.StatusConflict)
	default:
		http.Error(w, `{"error":"`+path+` changed since you last read it (it now hashes to `+
			wfdir.HashString(content)+`, the write expected `+*want+`)"}`, http.StatusConflict)
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONStatus is writeJSON with a chosen status, which http.Error cannot
// do: it forces text/plain, so a test staging an error that way proves nothing
// about how the CLI reads a real JSON error response.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// runCLI executes one `ronja ...` invocation against a fake instance from a
// working directory, returning whatever it printed on stdout.
//
// The whole command tree is rebuilt per call because the global flags are
// package vars bound by the root command — reusing one would carry --json from
// a previous test into the next.
func runCLI(t *testing.T, workdir string, args ...string) (string, error) {
	t.Helper()
	t.Chdir(workdir)

	stdout, restore := captureStdout(t)
	root := newRootCmd()
	root.SetArgs(args)
	// Cobra's own output goes to the test log, not the captured stdout, so a
	// usage dump cannot be mistaken for a command's JSON.
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	// The SAME context Execute builds, signal handler included: it is what makes
	// every command's cancellation branch reachable, so a test running the tree
	// without it would exercise a different program from the one that ships.
	ctx, stop := signalContext()
	defer stop()
	err := root.ExecuteContext(ctx)

	return restore(stdout), err
}

// failOnceWithATimeout makes the NEXT request to one method+path die on a
// deadline instead of reaching the fake, and lets everything after it through.
//
// A timeout is the one write failure a workflow push goes and asks the server
// about, so it is the only way into the reconcile — and the branch a mistake
// there turns into silent data loss. The alternatives were to wait a real
// 120-second deadline out or to leave it untested.
//
// Faked at the TRANSPORT rather than by a slow handler for two reasons: it costs
// no wall time, and the fake never sees the request at all, which is exactly the
// case being modelled — a write that timed out and did NOT land. `fired` is held
// across clients so the retry a test makes afterwards is an ordinary push.
func failOnceWithATimeout(t *testing.T, method, path string) {
	t.Helper()
	transport := &timeoutOnce{method: method, path: path}
	previous := newClient
	newClient = func(baseURL, token string) *api.Client {
		client := previous(baseURL, token)
		transport.base = client.HTTP.Transport
		client.HTTP.Transport = transport
		return client
	}
	t.Cleanup(func() { newClient = previous })
}

// landOnceThenTimeOut lets the NEXT request to one method+path reach the fake
// and COMMIT, then reports a deadline to the caller anyway.
//
// The other half of failOnceWithATimeout, and the half every reconcile is
// actually about: "the write timed out and did not land" is the easy branch, and
// the one that costs is "it landed and we stopped listening". Nothing in the
// error the client sees distinguishes the two, which is why the reconcile has to
// go and ask.
//
// Same transport-level trick and the same reason: it costs no wall time, and a
// 120-second deadline cannot be waited out in a test.
func landOnceThenTimeOut(t *testing.T, method, path string) {
	t.Helper()
	transport := &timeoutAfterLandingOnce{method: method, path: path}
	previous := newClient
	newClient = func(baseURL, token string) *api.Client {
		client := previous(baseURL, token)
		transport.base = client.HTTP.Transport
		client.HTTP.Transport = transport
		return client
	}
	t.Cleanup(func() { newClient = previous })
}

type timeoutAfterLandingOnce struct {
	base   http.RoundTripper
	method string
	path   string
	fired  bool
}

func (t *timeoutAfterLandingOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.fired || req.Method != t.method || req.URL.Path != t.path {
		return base.RoundTrip(req)
	}
	t.fired = true
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// The server's work is done; the client just never gets to read it.
	resp.Body.Close()
	return nil, context.DeadlineExceeded
}

type timeoutOnce struct {
	base   http.RoundTripper
	method string
	path   string
	fired  bool
}

func (t *timeoutOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.fired && req.Method == t.method && req.URL.Path == t.path {
		t.fired = true
		// http.Client wraps this in a *url.Error, which is what api.IsTimeout
		// unwraps — the same shape a per-request deadline produces.
		return nil, context.DeadlineExceeded
	}
	if t.base == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	return t.base.RoundTrip(req)
}

// signIn points the CLI at a fake instance with a credential in an isolated
// config dir, the way an actual login would leave things.
//
// RONJA_URL and RONJA_TOKEN rather than a seeded config.json: they are the
// documented agent/CI path, they outrank the file, and they leave nothing for a
// test ordering accident to inherit.
func signIn(t *testing.T, f *fakeInstance) {
	t.Helper()
	signInTo(t, f.URL())
}

// signInTo is signIn for a server that is not a fakeInstance — the `api` and
// `query` tests serve their own, because neither command has anything to do
// with the workflow surface fakeInstance models.
func signInTo(t *testing.T, url string) {
	t.Helper()
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	t.Setenv("RONJA_URL", url)
	t.Setenv("RONJA_TOKEN", "test-token")
}

// signOut is signIn without a credential: a resolvable instance the caller has
// no token for, which is a state `wf status` is required to survive.
//
// The config dir is fresh AND empty, so there is genuinely no profile to fall
// back on. Clearing RONJA_TOKEN alone does not sign you out — a stored profile
// would supply both a token and an organization, which is the opposite of the
// state these tests mean to create.
func signOut(t *testing.T, f *fakeInstance) {
	t.Helper()
	signOutFrom(t, f.URL())
}

// signOutFrom is signOut for a server that is not a fakeInstance — the
// data-app fake serves its own, and `app status` has the same
// works-signed-out requirement `wf status` does.
func signOutFrom(t *testing.T, url string) {
	t.Helper()
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
	t.Setenv("RONJA_URL", url)
	t.Setenv("RONJA_TOKEN", "")
	t.Setenv("RONJA_PROFILE", "")
}

// writeProfile seeds one profile into the isolated config dir, for the tests
// that need a stored ORGANIZATION rather than an environment credential — an
// environment token deliberately carries none. An empty token leaves the
// profile signed out, which is a legitimate state: the organization is still
// what a folder's binding is keyed by.
func writeProfile(t *testing.T, name, url, tenantID, token string) {
	t.Helper()
	dir := os.Getenv("RONJA_CONFIG_DIR")
	if dir == "" {
		t.Fatal("writeProfile needs an isolated RONJA_CONFIG_DIR — call signIn or signOut first")
	}
	f := &config.File{
		Current: name,
		Profiles: map[string]*config.Profile{
			name: {URL: url, TenantID: tenantID, Token: token},
		},
	}
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("encode profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.FileName), body, 0o600); err != nil {
		t.Fatalf("write %s: %v", config.FileName, err)
	}
}

// captureStdout swaps os.Stdout for a temp FILE rather than a pipe: a pipe
// whose buffer fills deadlocks the command under test, and these commands print
// unbounded file lists.
func captureStdout(t *testing.T) (*os.File, func(*os.File) string) {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatalf("create capture file: %v", err)
	}
	saved := os.Stdout
	os.Stdout = tmp
	return tmp, func(f *os.File) string {
		os.Stdout = saved
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("rewind capture file: %v", err)
		}
		body, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("read capture file: %v", err)
		}
		f.Close()
		return string(body)
	}
}

// decodeJSON parses a --json payload into a generic map, failing the test with
// the raw output when it is not JSON at all (which is how a command that
// printed a human report instead announces itself).
func decodeJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not JSON (%v):\n%s", err, out)
	}
	return payload
}

// marshalJSON renders a value as JSON so two of them can be compared as text.
// Used where the assertion is "this was not rewritten": a string diff names the
// field that moved, where reflect.DeepEqual only says false.
func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return string(body)
}

// readFile is os.ReadFile with the test's error handling.
func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(body)
}

// currentProfile reads the stored current-profile name back off disk, for the
// tests that assert a command changed it — or, more often, that a refused one
// did not.
func currentProfile(t *testing.T) string {
	t.Helper()
	f, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return f.Current
}
