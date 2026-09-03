package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The automation command harness.
//
// A SEPARATE fake from the other three, by the repo's convention, and here the
// surface really is different in the ways these tests are about:
//
//  1. LIST IS THE ONLY BULK READ, and it carries `total` beside the page. The
//     truncation refusal is scored off that field, so the fake reports it
//     honestly and can be told to report MORE than it returns — which is the
//     shape a feature past the page cap has.
//  2. THE ROW AND THE INPUT DISAGREE ABOUT TWO FIELD NAMES. The fake stores what
//     the INPUT sends and emits the ROW's spelling, exactly as production does,
//     so a CLI that compared like-named fields would report permanent drift here
//     rather than passing.
//  3. `updatedAt` IS THE WHOLE DRIFT ANCHOR. Every write moves it, and tests
//     move it by hand to model somebody editing in the web app.
//  4. THERE IS NO DRAFT AND NO VERSION, so there is nothing to check out and no
//     commit — a fake that modelled one would let a CLI that invented a
//     lifecycle pass.
type fakeAutomationInstance struct {
	t  *testing.T
	mu sync.Mutex

	// rows is every automation, keyed by id.
	rows map[string]*api.Automation
	// totalOverride, when non-zero, is what LIST reports as `total` regardless of
	// how many rows it returns — the truncation shape.
	totalOverride uint64
	// ignoreFeatureFilter models an OLDER backend: one that accepts `featureID`,
	// ignores it, and answers with everything the caller can see.
	ignoreFeatureFilter bool

	privilegeLevel int
	failMe         int

	// --- failure injection ---------------------------------------------------
	failList   int
	failCreate int
	failUpdate map[string]int
	failDelete map[string]int
	failGet    map[string]int
	// halfApply makes an UPDATE write the row's scalar fields and then answer an
	// error — the non-transactional shape §5.4 is about, where the schedule lands
	// and the references do not.
	halfApply map[string]bool

	// --- what the fake was asked to do ---------------------------------------
	created  []api.CreateAutomationInput
	updates  []recordedAutomationUpdate
	deleted  []string
	nextID   int
	Requests []string

	server *httptest.Server
}

// recordedAutomationUpdate is one PUT, with the row it was addressed to and the
// RAW body — raw, because half of what these tests assert is which KEYS were
// sent, and a decoded struct cannot tell an absent key from a zero value.
type recordedAutomationUpdate struct {
	ID   string
	Body map[string]json.RawMessage
}

func newFakeAutomationInstance(t *testing.T) *fakeAutomationInstance {
	t.Helper()
	f := &fakeAutomationInstance{
		t:              t,
		rows:           map[string]*api.Automation{},
		failUpdate:     map[string]int{},
		failDelete:     map[string]int{},
		failGet:        map[string]int{},
		halfApply:      map[string]bool{},
		privilegeLevel: 50,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAutomationInstance) URL() string { return f.server.URL }

func (f *fakeAutomationInstance) Key() wfdir.InstanceKey {
	return wfdir.InstanceKey{URL: f.server.URL, TenantID: testTenantID}
}

// AddAutomation registers a live row, filling in what every command reads so a
// test only states what it is about.
func (f *fakeAutomationInstance) AddAutomation(row *api.Automation) *api.Automation {
	f.t.Helper()
	if row.FeatureID == "" {
		row.FeatureID = "collection-1"
	}
	if row.TriggerKind == "" {
		row.TriggerKind = api.TriggerKindCron
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	}
	if row.Action == nil {
		row.Action = &api.AutomationAction{
			ID: "act-" + row.ID, ScheduledJobID: row.ID, Kind: api.ActionKindAgent,
		}
	}
	row.Normalize()
	f.rows[row.ID] = row
	return row
}

// RowOf reads one row back, for assertions.
func (f *fakeAutomationInstance) RowOf(id string) *api.Automation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id]
}

// Touch moves a row's updatedAt without changing anything else — somebody
// editing it in the web app, which is the only thing this loop's drift anchor
// can see.
func (f *fakeAutomationInstance) Touch(id string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.rows[id]; row != nil {
		row.UpdatedAt = at
	}
}

// Pause turns a row off with a reason and moves its updatedAt, which is exactly
// what an operator pausing a runaway automation leaves behind.
func (f *fakeAutomationInstance) Pause(id, reason string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.rows[id]; row != nil {
		row.Enabled, row.DisabledReason, row.UpdatedAt = false, reason, at
	}
}

func (f *fakeAutomationInstance) requestOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.Requests...)
}

func (f *fakeAutomationInstance) requestsMatching(substr string) int {
	n := 0
	for _, r := range f.requestOrder() {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

func (f *fakeAutomationInstance) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	if r.URL.Path == "/api/v2/authentication/me" {
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
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v2/automations")
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimPrefix(rest, "/")
	switch {
	case id == "" && r.Method == http.MethodGet:
		f.serveList(w, r)
	case id == "" && r.Method == http.MethodPost:
		f.serveCreate(w, r)
	case id != "" && r.Method == http.MethodGet:
		f.serveGet(w, id)
	case id != "" && r.Method == http.MethodPut:
		f.serveUpdate(w, r, id)
	case id != "" && r.Method == http.MethodDelete:
		f.serveDelete(w, id)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func (f *fakeAutomationInstance) serveList(w http.ResponseWriter, r *http.Request) {
	if f.failList != 0 {
		http.Error(w, `{"error":"boom"}`, f.failList)
		return
	}
	featureID := r.URL.Query().Get("featureID")
	var result []*api.Automation
	for _, id := range sortedRowIDs(f.rows) {
		row := f.rows[id]
		if !f.ignoreFeatureFilter && featureID != "" && row.FeatureID != featureID {
			continue
		}
		result = append(result, f.view(row))
	}
	total := uint64(len(result))
	if f.totalOverride != 0 {
		total = f.totalOverride
	}
	writeJSON(w, map[string]any{"token": "", "total": total, "result": result})
}

func (f *fakeAutomationInstance) serveGet(w http.ResponseWriter, id string) {
	if status := f.failGet[id]; status != 0 {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}
	row := f.rows[id]
	if row == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, f.view(row))
}

func (f *fakeAutomationInstance) serveCreate(w http.ResponseWriter, r *http.Request) {
	if f.failCreate != 0 {
		http.Error(w, `{"error":"boom"}`, f.failCreate)
		return
	}
	var in api.CreateAutomationInput
	decodeBody(f.t, r, &in)
	f.created = append(f.created, in)
	f.nextID++
	row := &api.Automation{
		ID:          fmt.Sprintf("job-%d", f.nextID),
		FeatureID:   in.FeatureID,
		Name:        in.Name,
		Description: in.Description,
		Prompt:      in.Prompt,
		TriggerKind: in.TriggerKind,
		CronExpr:    in.CronExpr,
		Timezone:    in.Timezone,
		// ⚠️ The two spelling swaps, exactly as production does them: what came
		// in under the INPUT name is emitted under the ROW name.
		EmailAllowedFromAddrs: in.EmailAllowedFromAddresses,
		WatchedTableIDs:       in.WatchedTableIds,
		EventName:             in.EventName,
		MailboxID:             in.MailboxID,
		Model:                 in.Model,
		ApprovedTools:         in.ApprovedTools,
		ApprovedToolGroups:    in.ApprovedToolGroups,
		ReferenceBased:        in.ReferenceBased == nil || *in.ReferenceBased,
		Enabled:               in.Enabled == nil || *in.Enabled,
		UpdatedAt:             time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
	}
	if in.TriggerKind == "" {
		row.TriggerKind = api.TriggerKindCron
	}
	row.EmailAllowedFromDomains = in.EmailAllowedFromDomains
	row.ReportingTimezone = in.ReportingTimezone
	row.RateLimitPerMinute, row.RateLimitPerDay = in.RateLimitPerMinute, in.RateLimitPerDay
	if in.MailboxFilter != nil {
		row.MailboxFilter = *in.MailboxFilter
	}
	row.Action = &api.AutomationAction{ID: "act-" + row.ID, ScheduledJobID: row.ID, Kind: api.ActionKindAgent}
	if in.Action != nil {
		row.Action.Kind = in.Action.Kind
		row.Action.Config.WorkflowID = in.Action.Config.WorkflowID
		row.Action.Config.AgentID = in.Action.Config.AgentID
		row.Action.Config.ParameterValues = in.Action.Config.ParameterValues
		if in.Action.Kind == api.ActionKindSavedAgent {
			row.Action.Config.Prompt = in.Action.Config.Prompt
		}
	}
	// ⚠️ ONLY AN INLINE AGENT ACTION KEEPS THEM. Create writes references on an
	// `else if` branch a workflow or saved_agent action never reaches, and only an
	// agent automation reads them back at all (its config is synthesized from the
	// job's own reference rows; the other two kinds store a fixed config shape
	// with no room for one). A fake that carried them across every kind would
	// model the CLI's intent instead of the server's behaviour, and a folder
	// declaring references a workflow action silently drops would pass here and
	// report unfixable drift in production.
	if row.Action.Kind == api.ActionKindAgent {
		row.Action.Config.References = in.References
	}
	row.Normalize()
	f.rows[row.ID] = row
	writeJSON(w, f.view(row))
}

func (f *fakeAutomationInstance) serveUpdate(w http.ResponseWriter, r *http.Request, id string) {
	row := f.rows[id]
	if row == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var raw map[string]json.RawMessage
	var patch api.AutomationPatch
	decodeBody(f.t, r, &raw)
	if body, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(body, &patch)
	}
	f.updates = append(f.updates, recordedAutomationUpdate{ID: id, Body: raw})
	if status := f.failUpdate[id]; status != 0 && !f.halfApply[id] {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}

	if patch.Name != nil {
		row.Name = *patch.Name
	}
	if patch.Description != nil {
		row.Description = *patch.Description
	}
	if patch.Prompt != nil {
		row.Prompt = *patch.Prompt
	}
	if patch.CronExpr != nil {
		row.CronExpr = *patch.CronExpr
	}
	if patch.Timezone != nil {
		row.Timezone = *patch.Timezone
	}
	if patch.ReportingTimezone != nil {
		row.ReportingTimezone = *patch.ReportingTimezone
	}
	if patch.Enabled != nil {
		row.Enabled = *patch.Enabled
		if row.Enabled {
			row.DisabledReason = ""
		}
	}
	if patch.Model != nil {
		row.Model = *patch.Model
	}
	if patch.ApprovedTools != nil {
		row.ApprovedTools = *patch.ApprovedTools
	}
	if patch.ApprovedToolGroups != nil {
		row.ApprovedToolGroups = *patch.ApprovedToolGroups
	}
	if patch.EmailAllowedFromDomains != nil {
		row.EmailAllowedFromDomains = *patch.EmailAllowedFromDomains
	}
	if patch.EmailAllowedFromAddresses != nil {
		row.EmailAllowedFromAddrs = *patch.EmailAllowedFromAddresses
	}
	if patch.WatchedTableIds != nil {
		row.WatchedTableIDs = *patch.WatchedTableIds
	}
	if patch.MailboxID != nil {
		row.MailboxID = *patch.MailboxID
	}
	if patch.MailboxFilter != nil {
		row.MailboxFilter = *patch.MailboxFilter
	}
	if patch.RateLimitPerMinute != nil {
		row.RateLimitPerMinute = patch.RateLimitPerMinute
	}
	if patch.RateLimitPerDay != nil {
		row.RateLimitPerDay = patch.RateLimitPerDay
	}
	row.UpdatedAt = row.UpdatedAt.Add(time.Hour)

	// ⚠️ THE HALF-APPLY. The scalar fields above are already written; the
	// reference and action writes are a SEPARATE server-side call, and this
	// models it failing. Nothing in the error tells the caller which half landed,
	// which is the whole point.
	if f.halfApply[id] {
		http.Error(w, `{"error":"setting the references failed"}`, http.StatusInternalServerError)
		return
	}
	// ⚠️ WRITTEN ALWAYS, READABLE ONLY BEHIND AN INLINE AGENT ACTION — the second
	// door of the same bug. A body carrying no workflow/saved_agent action reaches
	// the server's reference branch and the rows really are written, but the read
	// decodes the config off the STORED action JSON, which for a workflow or
	// saved_agent action has nowhere to keep them. So a body that sends
	// references at a workflow row succeeds and changes nothing anybody can read.
	// A fake that stored them here would model the write and not the read, and
	// the folder's permanent drift would pass.
	if patch.References != nil && row.Action != nil && row.Action.Kind == api.ActionKindAgent {
		row.Action.Config.References = *patch.References
	}
	if patch.Action != nil {
		// ⚠️ THE CONFIG IS REPLACED WHOLESALE, because that is what the server
		// does: writeActionTx DELETEs the job's action row and INSERTs a new one
		// from the config the body carried, so a field the body left out is GONE
		// rather than left as it was. A fake that patched the config field by
		// field would model the CLI's idea of an update instead of the server's,
		// and the one bug this surface can have — an unmanaged config field
		// silently cleared by a push that changed a different one — would pass
		// here and ship.
		//
		// ⚠️ REFERENCES ARE NOT CARRIED ACROSS EITHER, for the same reason:
		// SetWorkflowAction and SetSavedAgentAction each DELETE the job's
		// reference rows before writing the action, and the branch that would
		// write new ones is only reached when the body carries no action of those
		// kinds. Only an inline agent action keeps them.
		refs := row.Action.Config.References
		switch patch.Action.Kind {
		case api.ActionKindWorkflow:
			row.Action = &api.AutomationAction{
				ID: "act-" + row.ID, ScheduledJobID: row.ID, Kind: patch.Action.Kind,
			}
			row.Action.Config.WorkflowID = patch.Action.Config.WorkflowID
			row.Action.Config.ParameterValues = patch.Action.Config.ParameterValues
		case api.ActionKindSavedAgent:
			row.Action = &api.AutomationAction{
				ID: "act-" + row.ID, ScheduledJobID: row.ID, Kind: patch.Action.Kind,
			}
			row.Action.Config.AgentID = patch.Action.Config.AgentID
			row.Action.Config.Prompt = patch.Action.Config.Prompt
		default:
			// ⚠️ AN AGENT ACTION IS NOT WRITTEN BY THIS ROUTE AT ALL. The update
			// handler writes a child action row for a workflow or a saved_agent
			// and NOTHING ELSE — there is no SetAgentAction anywhere in the
			// backend — so a body carrying kind "agent" leaves the child row
			// exactly as it was and the row KEEPS THE KIND IT HAD. An action that
			// is ALREADY agent is still refreshed, but by rscheduledjob.Update's
			// own re-derive off the row's top-level fields (which runs only when
			// the current kind is agent or absent), not by the body's
			// action.config — so the body's config is ignored either way.
			//
			// A fake that switched the kind here would model a write the server
			// cannot do, and the permanent drift refuseUnpatchableAutomation
			// refuses would push green in every test.
			if row.Action.Kind != api.ActionKindAgent {
				break
			}
			row.Action = &api.AutomationAction{
				ID: "act-" + row.ID, ScheduledJobID: row.ID, Kind: api.ActionKindAgent,
			}
			row.Action.Config.References = refs
			row.Action.Config.Prompt = row.Prompt
			row.Action.Config.ReferenceBased = row.ReferenceBased
			row.Action.Config.ApprovedTools = row.ApprovedTools
			row.Action.Config.ApprovedToolGroups = row.ApprovedToolGroups
		}
	}
	row.Normalize()
	writeJSON(w, f.view(row))
}

func (f *fakeAutomationInstance) serveDelete(w http.ResponseWriter, id string) {
	if status := f.failDelete[id]; status != 0 {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}
	if f.rows[id] == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	f.deleted = append(f.deleted, id)
	delete(f.rows, id)
	writeJSON(w, map[string]any{"ok": true})
}

// view is a COPY, so a handler's response cannot be mutated through the stored
// row by a later write in the same test.
func (f *fakeAutomationInstance) view(row *api.Automation) *api.Automation {
	out := *row
	if row.Action != nil {
		action := *row.Action
		out.Action = &action
	}
	return &out
}

func sortedRowIDs(rows map[string]*api.Automation) []string {
	out := make([]string, 0, len(rows))
	for id := range rows {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// signInAutomation points the CLI at a fake automation instance with a
// credential in an isolated config dir.
func signInAutomation(t *testing.T, f *fakeAutomationInstance) {
	t.Helper()
	signInTo(t, f.URL())
}

// runAutomationCLI is runCLI with stderr captured as well: this loop says most
// of what it refuses on stderr, and a test that could not read it would be
// asserting only that the command did not crash.
func runAutomationCLI(t *testing.T, workdir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, workdir, args...)
	})
	return stdout, stderr, err
}

// writeAutomationFolder lays out an automation folder on disk: a manifest of the
// right kind, an optional stack binding, and the given files.
func writeAutomationFolder(t *testing.T, dir string, manifest *wfdir.Manifest, lock *wfdir.Lock, files map[string]string) string {
	t.Helper()
	if manifest.Kind == "" {
		manifest.Kind = wfdir.KindAutomation
	}
	if manifest.Title == "" {
		manifest.Title = "Orders automations"
	}
	if lock == nil {
		lock = &wfdir.Lock{}
	}
	if err := wfdir.SaveFolder(dir, manifest, lock); err != nil {
		t.Fatalf("write folder: %v", err)
	}
	for path, content := range files {
		if err := wfdir.WriteFile(dir, path, content); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir
}

// automationStack builds the manifest + lock pair for a folder bound to one
// stack, which is what every folder this loop creates looks like.
func automationStack(f *fakeAutomationInstance, featureID string, bound map[string]string, seen map[string]string) (*wfdir.Manifest, *wfdir.Lock) {
	manifest := &wfdir.Manifest{
		Kind:  wfdir.KindAutomation,
		Title: "Orders automations",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: featureID},
		},
	}
	lock := &wfdir.Lock{}
	if len(bound) > 0 {
		lock.SetBinding("prod", wfdir.Binding{Automations: bound})
		for path, id := range bound {
			lock.SetAutomationSeen("prod", path, id, seen[path])
		}
	}
	return manifest, lock
}

// automationBindingOf reads a folder's recorded binding back off disk.
func automationBindingOf(t *testing.T, root string, key wfdir.InstanceKey) wfdir.Binding {
	t.Helper()
	manifest, err := wfdir.LoadManifest(root, wfdir.AutomationKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	sel, err := manifest.Select(lock, key, "")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	return sel.Binding
}

// automationSeenOf reads the drift anchor a folder recorded for one path.
func automationSeenOf(t *testing.T, root, stack, path string) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	_, seen := lock.AutomationSeen(stack, path)
	return seen
}
