package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/checkfile"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The HEALTH-CHECK half of the pipeline loop: `checks/<stem>.json`.

// --- the fake ---------------------------------------------------------------

// fakeChecks is the fake instance's health-check state, modelled on
// api_model_check_authoring.go and rtablecheck's store rules that the CLI's
// ordering depends on: one enabled watchdog per table, a case-insensitive unique
// name, fail checks admin-protected.
type fakeChecks struct {
	rows   map[string][]*api.TableCheck
	nextID int
	writes []recordedCheckWrite
	// unsupported makes POST answer what an older server's router answers for
	// an unmatched route: a plain-text 404 breadcrumb.
	unsupported bool
	// oldPut makes PUT behave like the older silence route: `enabled` required,
	// every other field silently dropped, 200.
	oldPut bool
	// notBuilt answers 409 table_not_built to a write that would run an
	// expression on these tables.
	notBuilt map[string]bool
	// nonAdmin refuses, 403, a write that creates, silences or re-severities a
	// fail check.
	nonAdmin bool
	// raceOnCreate: a colleague's create of this (folded) name lands just before
	// ours, so ours answers 409 name_taken.
	raceOnCreate map[string]bool
	// onList runs on each GET of a table's checks (with the fake's lock held).
	onList func()
	// failPut answers every PUT with this status and a JSON error; 0 is off.
	failPut int
}

type recordedCheckWrite struct {
	Method, TableID, CheckID string
	Body                     map[string]any
}

// AddCheck puts a check on a table as if the app or chat had created it.
func (f *fakePipelineInstance) AddCheck(tableID string, check api.TableCheck) *api.TableCheck {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addCheckLocked(tableID, check)
}

func (f *fakePipelineInstance) addCheckLocked(tableID string, check api.TableCheck) *api.TableCheck {
	c := &f.checkState
	if c.rows == nil {
		c.rows = map[string][]*api.TableCheck{}
	}
	c.nextID++
	check.ID = fmt.Sprintf("tcheck-%d", c.nextID)
	check.TableID = tableID
	if check.Kind == "" {
		check.Kind = api.TableCheckKindExpression
		if check.ExpectedIntervalSeconds > 0 {
			check.Kind = api.TableCheckKindFreshness
		}
	}
	if check.Severity == "" {
		check.Severity = "warn"
	}
	row := check
	c.rows[tableID] = append(c.rows[tableID], &row)
	return &row
}

func (f *fakePipelineInstance) checksOn(tableID string) []*api.TableCheck {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkState.rows[tableID]
}

func (f *fakePipelineInstance) checkNamed(tableID, name string) *api.TableCheck {
	for _, row := range f.checksOn(tableID) {
		if row.Name == name {
			return row
		}
	}
	return nil
}

func (f *fakePipelineInstance) checkWrites() []recordedCheckWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.checkState.writes)
}

func checkVerdict(expression string) string {
	if strings.Contains(expression, "false") {
		return "fail"
	}
	return "pass"
}

func (f *fakePipelineInstance) writeCheck(w http.ResponseWriter, row *api.TableCheck, updated, reRan bool) {
	out := api.TableCheckWriteResult{TableCheck: *row, Updated: updated, ReRan: reRan}
	if reRan {
		out.Verdict = &api.TableCheckVerdict{Status: checkVerdict(row.Expression)}
	}
	writeJSON(w, out)
}

// serveChecks is GET/POST /feature/model/:id/checks and PUT …/checks/:checkID.
// Called with f.mu held.
func (f *fakePipelineInstance) serveChecks(w http.ResponseWriter, r *http.Request, tableID, checkID string) {
	c := &f.checkState
	if f.tables[tableID] == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var body map[string]any
	if r.Method != http.MethodGet {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
	}
	str := func(key string) (string, bool) {
		v, ok := body[key].(string)
		return v, ok
	}
	enabledCount := func(kind string) int {
		n := 0
		for _, row := range c.rows[tableID] {
			if row.Enabled && row.Kind == kind {
				n++
			}
		}
		return n
	}
	taken := func(name, except string) bool {
		for _, row := range c.rows[tableID] {
			if row.ID != except && checkfile.Fold(row.Name) == checkfile.Fold(name) {
				return true
			}
		}
		return false
	}
	nameTaken := func() {
		http.Error(w, `{"error":"this table already has a check with that name","code":"name_taken"}`, http.StatusConflict)
	}

	switch {
	case checkID == "" && r.Method == http.MethodGet:
		if c.onList != nil {
			c.onList()
			// Give an interrupt time to cancel the caller's context before
			// this answer reaches it.
			time.Sleep(200 * time.Millisecond)
		}
		out := make([]api.TableCheck, 0, len(c.rows[tableID]))
		for _, row := range c.rows[tableID] {
			out = append(out, *row)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, out)

	case checkID == "" && r.Method == http.MethodPost:
		if c.unsupported {
			http.Error(w, "404 — no matching route.\nAPI index for agents: /llms.txt\n", http.StatusNotFound)
			return
		}
		c.writes = append(c.writes, recordedCheckWrite{Method: "POST", TableID: tableID, Body: body})
		name, _ := str("name")
		severity, hasSev := str("severity")
		if !hasSev {
			severity = "warn"
		}
		if c.nonAdmin && severity == "fail" {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		expr, hasExpr := str("expression")
		if hasExpr && c.notBuilt[tableID] {
			http.Error(w, `{"error":"this table has no data yet","code":"table_not_built"}`, http.StatusConflict)
			return
		}
		if c.raceOnCreate[checkfile.Fold(name)] {
			delete(c.raceOnCreate, checkfile.Fold(name))
			f.addCheckLocked(tableID, api.TableCheck{Name: name, Expression: "count(*) >= 0", Enabled: true})
			nameTaken()
			return
		}
		if taken(name, "") {
			nameTaken()
			return
		}
		enabled := true
		if v, ok := body["enabled"].(bool); ok {
			enabled = v
		}
		check := api.TableCheck{Name: name, Expression: expr, Severity: severity, Enabled: enabled}
		if hours, ok := body["expectedIntervalHours"].(float64); ok {
			// The one-watchdog index binds ENABLED rows only.
			if enabled && enabledCount(api.TableCheckKindFreshness) > 0 {
				http.Error(w, `{"error":"this table already has an enabled freshness watchdog"}`, http.StatusBadRequest)
				return
			}
			check.ExpectedIntervalSeconds = int(hours) * 3600
		}
		if desc, ok := str("description"); ok {
			check.Description = desc
		}
		row := f.addCheckLocked(tableID, check)
		f.writeCheck(w, row, true, true)

	case checkID != "" && r.Method == http.MethodPut:
		var row *api.TableCheck
		for _, candidate := range c.rows[tableID] {
			if candidate.ID == checkID {
				row = candidate
			}
		}
		if row == nil {
			http.Error(w, `{"error":"check not found on this table"}`, http.StatusNotFound)
			return
		}
		c.writes = append(c.writes, recordedCheckWrite{Method: "PUT", TableID: tableID, CheckID: checkID, Body: body})
		if c.failPut != 0 {
			http.Error(w, `{"error":"forbidden"}`, c.failPut)
			return
		}
		enabled, hasEnabled := body["enabled"].(bool)
		if c.oldPut {
			if !hasEnabled {
				http.Error(w, `{"error":"enabled is required"}`, http.StatusBadRequest)
				return
			}
			row.Enabled = enabled
			writeJSON(w, row)
			return
		}
		severity, hasSev := str("severity")
		if c.nonAdmin && ((row.Severity == "fail" && ((hasSev && severity != "fail") || (hasEnabled && !enabled))) || (hasSev && severity == "fail")) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		expr, hasExpr := str("expression")
		if hasExpr && c.notBuilt[tableID] {
			http.Error(w, `{"error":"this table has no data yet","code":"table_not_built"}`, http.StatusConflict)
			return
		}
		if name, ok := str("name"); ok {
			if taken(name, row.ID) {
				nameTaken()
				return
			}
			row.Name = name
		}
		reRan := false
		if hasExpr {
			row.Expression, reRan = expr, true
		}
		if hours, ok := body["expectedIntervalHours"].(float64); ok {
			row.ExpectedIntervalSeconds, reRan = int(hours)*3600, true
		}
		if hasSev {
			row.Severity = severity
		}
		if desc, ok := str("description"); ok {
			row.Description = desc
		}
		if hasEnabled {
			if enabled && !row.Enabled && row.Kind == api.TableCheckKindFreshness && enabledCount(api.TableCheckKindFreshness) > 0 {
				http.Error(w, `{"error":"this table already has an enabled freshness watchdog"}`, http.StatusBadRequest)
				return
			}
			row.Enabled = enabled
		}
		f.writeCheck(w, row, true, reRan)

	default:
		http.Error(w, "404 — no matching route.\n", http.StatusNotFound)
	}
}

// --- folder helpers ---------------------------------------------------------

const glSQL = "SELECT 1 AS id\n"

// seedChecksFolder lays out a folder that builds one LIVE, synced table
// (gl_lines.sql → table-gl) and declares its checks. The SQL is unchanged, so
// the checks pass is the only thing a push does.
func seedChecksFolder(t *testing.T, f *fakePipelineInstance, checks string) string {
	t.Helper()
	f.AddFeature("collection-1", "Ekonomi", "private")
	f.AddTable(&api.Table{ID: "table-gl", Name: "gl_lines", FeatureID: "collection-1", Code: glSQL})
	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Ekonomi"}
	manifest.SetBinding(f.Key(), wfdir.Binding{FeatureID: "collection-1", Tables: map[string]string{"gl_lines.sql": "table-gl"}})
	writePipelineFolder(t, root, manifest, map[string]string{"gl_lines.sql": glSQL, "checks/gl_lines.json": checks})
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"gl_lines.sql": glSQL},
		map[string]wfdir.TableState{"gl_lines.sql": {TableID: "table-gl"}})
	return root
}

func writeChecksFile(t *testing.T, root, body string) {
	t.Helper()
	if err := wfdir.WriteFile(root, "checks/gl_lines.json", body); err != nil {
		t.Fatalf("write checks: %v", err)
	}
}

const twoChecks = `{"checks": [
  {"name": "Unik rad per id", "expression": "count(*) = count(distinct id)", "severity": "fail", "description": "Dubbletter."},
  {"name": "Bygger om dagligen", "expectedIntervalHours": 24}
]}`

// checksOf decodes a push/publish --json payload's checks[] for one file.
func checksOf(t *testing.T, out string) []pipelineChecksResult {
	t.Helper()
	var payload struct {
		Checks []pipelineChecksResult `json:"checks"`
	}
	decodeJSONInto(t, out, &payload)
	return payload.Checks
}

func checkOutcomes(r pipelineChecksResult) map[string]string {
	out := map[string]string{}
	for _, c := range r.Checks {
		out[c.Name] = c.Outcome
	}
	return out
}

func managedOf(t *testing.T, root string, key wfdir.InstanceKey) map[string]wfdir.LockCheckEntry {
	t.Helper()
	return pipelineStateOf(t, root, key).Checks["checks/gl_lines.json"].Managed
}

// --- tests ------------------------------------------------------------------

// Checks on a table whose SQL did not change are applied by a bare push — the
// main case — and recorded per check; a re-push writes nothing.
func TestPipelineChecksPushCreatesThenReRunIsANoOp(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
	}
	results := checksOf(t, out)
	if len(results) != 1 || results[0].TableID != "table-gl" {
		t.Fatalf("checks = %+v", results)
	}
	if got := checkOutcomes(results[0]); got["Unik rad per id"] != checksOutcomeApplied || got["Bygger om dagligen"] != checksOutcomeApplied {
		t.Fatalf("outcomes = %v", got)
	}
	if results[0].Checks[0].Verdict == "" {
		t.Errorf("a created check's verdict must be reported: %+v", results[0].Checks[0])
	}
	if n := len(f.checksOn("table-gl")); n != 2 {
		t.Fatalf("checks on the table = %d", n)
	}
	// Only the declared fields went over the wire: the watchdog declares no
	// severity, so none was sent and the server default applies.
	for _, write := range f.checkWrites() {
		if write.Body["name"] == "Bygger om dagligen" {
			if _, sent := write.Body["severity"]; sent {
				t.Errorf("an undeclared severity was sent: %v", write.Body)
			}
		}
	}
	managed := managedOf(t, root, f.Key())
	if len(managed) != 2 || managed["unik rad per id"].CheckID == "" || managed["unik rad per id"].LiveSHA256 == "" {
		t.Fatalf("managed = %+v", managed)
	}

	before := len(f.checkWrites())
	out, stderr, err = runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("re-push: %v\n%s", err, stderr)
	}
	if got := len(f.checkWrites()) - before; got != 0 {
		t.Fatalf("an idempotent re-push wrote %d time(s)", got)
	}
	if got := checkOutcomes(checksOf(t, out)[0]); got["Unik rad per id"] != checksOutcomeUnchanged {
		t.Errorf("outcomes = %v", got)
	}
	if !decodeJSON(t, out)["upToDate"].(bool) {
		t.Error("a push whose checks all match must report upToDate")
	}
}

// An edited expression is an update in place of only that field — never an
// unchanged severity beside it, which would trip the admin gate on a fail check.
func TestPipelineChecksEditSendsOnlyTheDifferingField(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	id := f.checkNamed("table-gl", "Unik rad per id").ID
	writeChecksFile(t, root, strings.Replace(twoChecks, "count(distinct id)", "count(distinct (id))", 1))

	before := len(f.checkWrites())
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	writes := f.checkWrites()[before:]
	if len(writes) != 1 || writes[0].Method != "PUT" || writes[0].CheckID != id {
		t.Fatalf("writes = %+v", writes)
	}
	if len(writes[0].Body) != 1 || writes[0].Body["expression"] != "count(*) = count(distinct (id))" {
		t.Fatalf("the PUT must carry only the edited field: %v", writes[0].Body)
	}
	result := checksOf(t, out)[0]
	for _, c := range result.Checks {
		if c.Name == "Unik rad per id" && (c.Outcome != checksOutcomeApplied || c.Verdict != "pass" || !slices.Equal(c.Changed, []string{"expression"})) {
			t.Errorf("result = %+v", c)
		}
	}
}

// A table this push staged a draft for is pending publish, and the publish that
// commits it applies the checks.
func TestPipelineChecksNewTableDeferredThenAppliedOnPublish(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if err := wfdir.WriteFile(root, "gl_lines.sql", "SELECT 2 AS id\n"); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if got := checksOf(t, out)[0]; got.Outcome != checksOutcomePendingPublish {
		t.Fatalf("a table with an open draft must hold its checks: %+v", got)
	}
	if n := len(f.checkWrites()); n != 0 {
		t.Fatalf("pending checks were written: %d", n)
	}

	out, stderr, err = runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s\n%s", err, out, stderr)
	}
	results := checksOf(t, out)
	if len(results) != 1 || results[0].Outcome != "" || checkOutcomes(results[0])["Unik rad per id"] != checksOutcomeApplied {
		t.Fatalf("publish must apply the checks once the commit lands: %+v", results)
	}
	if n := len(f.checksOn("table-gl")); n != 2 {
		t.Errorf("checks on the table = %d", n)
	}
}

// A publish that goes to review leaves the checks pending.
func TestPipelineChecksReviewRouteLeavesThemPending(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	f.features["collection-1"].Scope = scopeOrganization
	f.privilegeLevel = 50 // an ordinary user: a shared-feature publish goes to review
	if err := wfdir.WriteFile(root, "gl_lines.sql", "SELECT 2 AS id\n"); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	results := checksOf(t, out)
	if len(results) != 1 || results[0].Outcome != checksOutcomePendingPublish {
		t.Fatalf("a review-route publish must leave the checks pending: %+v", results)
	}
	if n := len(f.checkWrites()); n != 0 {
		t.Errorf("checks were written for a table nobody committed: %d", n)
	}
}

// The server's 409 table_not_built (a live row that never built) is pending, not
// a refusal, and the push exits zero.
func TestPipelineChecksTableNotBuiltIsPending(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)"}]}`)
	f.checkState.notBuilt = map[string]bool{"table-gl": true}
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if got := checkOutcomes(checksOf(t, out)[0]); got["Unik rad per id"] != checksOutcomePendingPublish {
		t.Fatalf("outcomes = %v", got)
	}
	// A never-built table can also be one whose draft was discarded, with
	// nothing left to publish: the message says what to do, not "wait".
	if msg := checksOf(t, out)[0].Checks[0].Error; !strings.Contains(msg, "push the table's SQL and publish it") {
		t.Errorf("message = %q", msg)
	}
	if len(managedOf(t, root, f.Key())) != 0 {
		t.Error("a check that was not stored must not be recorded")
	}
}

// A same-named check that pre-dates the file is ADOPTED; rewriting one is said
// apart. A check neither declared nor owned is left alone and listed.
func TestPipelineChecksAdoptAndUnmanaged(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	f.AddCheck("table-gl", api.TableCheck{Name: "Unik rad per id", Expression: "count(*) = count(distinct id)", Severity: "fail", Description: "Dubbletter.", Enabled: true})
	f.AddCheck("table-gl", api.TableCheck{Name: "Bygger om dagligen", ExpectedIntervalSeconds: 48 * 3600, Enabled: true})
	f.AddCheck("table-gl", api.TableCheck{Name: "Kollegans check", Expression: "true", Enabled: true})

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	result := checksOf(t, out)[0]
	got := checkOutcomes(result)
	if got["Unik rad per id"] != checksOutcomeAdopted || got["Bygger om dagligen"] != checksOutcomeAdoptedChanged {
		t.Fatalf("outcomes = %v", got)
	}
	if !slices.Equal(result.Unmanaged, []string{"Kollegans check"}) {
		t.Errorf("unmanaged = %v", result.Unmanaged)
	}
	for _, w := range f.checkWrites() {
		if w.CheckID == f.checkNamed("table-gl", "Kollegans check").ID {
			t.Errorf("an unmanaged check was written: %+v", w)
		}
	}
	if len(managedOf(t, root, f.Key())) != 2 {
		t.Errorf("adopted checks must be recorded: %+v", managedOf(t, root, f.Key()))
	}
}

// A declared check whose live row is SILENCED, with `enabled` undeclared, is
// `silenced` — never `unchanged` — so nobody believes it guards the table.
func TestPipelineChecksSilencedIsNotUnchanged(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"Unik rad per id","expression":"true"}]}`)
	f.AddCheck("table-gl", api.TableCheck{Name: "Unik rad per id", Expression: "true", Enabled: false})
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	c := checksOf(t, out)[0].Checks[0]
	if c.Outcome != checksOutcomeSilenced || !strings.Contains(c.Detail, `"enabled": true`) {
		t.Fatalf("result = %+v", c)
	}
	if n := len(f.checkWrites()); n != 0 {
		t.Errorf("a silenced check was re-enabled by a file that does not say enabled: %d write(s)", n)
	}
}

// A check this folder owns, changed in the app since, is drift: refused with no
// write, and --force overwrites.
func TestPipelineChecksDriftRefusedThenForced(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	f.checkNamed("table-gl", "Unik rad per id").Expression = "count(*) > 0" // edited in chat
	writeChecksFile(t, root, strings.Replace(twoChecks, `"Dubbletter."`, `"Dubbletter betyder fel."`, 1))

	before := len(f.checkWrites())
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("drift must exit non-zero")
	}
	if got := checksOf(t, out)[0]; got.Outcome != checksOutcomeDrift || !strings.Contains(got.Error, "Unik rad per id") {
		t.Fatalf("result = %+v", got)
	}
	if n := len(f.checkWrites()) - before; n != 0 {
		t.Fatalf("drift refused but %d write(s) happened", n)
	}

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--force"); err != nil {
		t.Fatalf("push --force: %v\n%s", err, stderr)
	}
	if got := f.checkNamed("table-gl", "Unik rad per id"); got.Expression != "count(*) = count(distinct id)" || got.Description != "Dubbletter betyder fel." {
		t.Errorf("--force must overwrite: %+v", got)
	}
}

// A check the folder created and the file no longer declares is an orphan: the
// table's checks are refused until --prune.
func TestPipelineChecksOrphanRefusedWithoutPrune(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	writeChecksFile(t, root, `{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)","severity":"fail","description":"Dubbletter."}]}`)
	before := len(f.checkWrites())
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("an orphan must exit non-zero")
	}
	if got := checksOf(t, out)[0]; got.Outcome != checksOutcomeOrphaned || !strings.Contains(got.Error, "Bygger om dagligen") {
		t.Fatalf("result = %+v", got)
	}
	if n := len(f.checkWrites()) - before; n != 0 {
		t.Errorf("an orphan refusal wrote %d time(s)", n)
	}
	if !f.checkNamed("table-gl", "Bygger om dagligen").Enabled {
		t.Error("an orphan was silenced without --prune")
	}
}

// Renaming a freshness watchdog succeeds in ONE push with --prune, because the
// old one is silenced BEFORE the new one is created — the server allows one
// enabled watchdog per table.
func TestPipelineChecksPruneSilencesBeforeCreating(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	oldID := f.checkNamed("table-gl", "Bygger om dagligen").ID
	writeChecksFile(t, root, strings.Replace(twoChecks, "Bygger om dagligen", "Byggs varje dag", 1))

	before := len(f.checkWrites())
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--prune", "--json")
	if err != nil {
		t.Fatalf("push --prune: %v\n%s\n%s", err, out, stderr)
	}
	writes := f.checkWrites()[before:]
	if len(writes) != 2 || writes[0].Method != "PUT" || writes[0].CheckID != oldID || writes[0].Body["enabled"] != false || writes[1].Method != "POST" {
		t.Fatalf("the orphan must be silenced before the create: %+v", writes)
	}
	if f.checkNamed("table-gl", "Byggs varje dag") == nil || f.checkNamed("table-gl", "Bygger om dagligen").Enabled {
		t.Fatal("the rename did not land")
	}
	managed := managedOf(t, root, f.Key())
	if _, still := managed["bygger om dagligen"]; still || managed["byggs varje dag"].CheckID == "" {
		t.Errorf("managed = %+v", managed)
	}
}

// A maintainer's push lands the warn checks and is refused each fail one with
// the admin wording; only the refused one is retried next time.
func TestPipelineChecksNonAdminKeepsWhatLanded(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	f.checkState.nonAdmin = true

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a refused fail check must exit non-zero")
	}
	result := checksOf(t, out)[0]
	for _, c := range result.Checks {
		switch c.Name {
		case "Unik rad per id":
			if c.Outcome != checksOutcomeRefused || !strings.Contains(c.Error, "needs admin for a fail check") {
				t.Errorf("fail check = %+v", c)
			}
		case "Bygger om dagligen":
			if c.Outcome != checksOutcomeApplied {
				t.Errorf("warn check = %+v", c)
			}
		}
	}
	managed := managedOf(t, root, f.Key())
	if len(managed) != 1 || managed["bygger om dagligen"].CheckID == "" {
		t.Fatalf("the landed check must be recorded and only it: %+v", managed)
	}

	f.checkState.nonAdmin = false
	before := len(f.checkWrites())
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("admin push: %v\n%s", err, stderr)
	}
	writes := f.checkWrites()[before:]
	if len(writes) != 1 || writes[0].Body["name"] != "Unik rad per id" {
		t.Fatalf("only the refused check should be retried: %+v", writes)
	}
}

// An older server: the POST route is unmatched (plain-text breadcrumb) →
// unsupported; a JSON 404 (a table the caller cannot reach) → refused.
func TestPipelineChecksUnsupportedVersusRefused(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	f.checkState.unsupported = true
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("unsupported must exit non-zero")
	}
	if got := checksOf(t, out)[0]; got.Outcome != checksOutcomeUnsupported || !strings.Contains(got.Error, "upgrade the server") {
		t.Fatalf("result = %+v", got)
	}

	failure := classifyCheckWrite(&api.Error{Status: 404, Body: `{"error":"not found"}`}, "checks/gl_lines.json")
	if failure.outcome != checksOutcomeRefused || !failure.wholeTable {
		t.Errorf("a JSON 404 must be a per-table refusal: %+v", failure)
	}
	failure = classifyCheckWrite(&api.Error{Status: 404, Body: "404 — no matching route."}, "checks/gl_lines.json")
	if failure.outcome != checksOutcomeUnsupported {
		t.Errorf("the breadcrumb must be unsupported: %+v", failure)
	}
	failure = classifyCheckWrite(&api.Error{Status: 405, Body: ""}, "checks/gl_lines.json")
	if failure.outcome != checksOutcomeUnsupported {
		t.Errorf("a 405 must be unsupported: %+v", failure)
	}
}

// An older server's PUT answers 200 and drops every field but `enabled`; the
// read-back catches it as not_applied, and nothing is recorded.
func TestPipelineChecksVerifyCatchesASilentlyDroppedEdit(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)","enabled":true}]}`)
	f.AddCheck("table-gl", api.TableCheck{Name: "Unik rad per id", Expression: "count(*) > 0", Enabled: false})
	f.checkState.oldPut = true
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("not_applied must exit non-zero")
	}
	c := checksOf(t, out)[0].Checks[0]
	if c.Outcome != checksOutcomeNotApplied {
		t.Fatalf("result = %+v", c)
	}
	if len(managedOf(t, root, f.Key())) != 0 {
		t.Error("an unverified write must not be recorded")
	}
}

// Losing a create race (409 name_taken) re-reads and re-enters the diff for the
// check that won, rather than reporting a refusal or recording it unverified.
func TestPipelineChecksNameTakenRaceReEntersTheDiff(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)"}]}`)
	f.checkState.raceOnCreate = map[string]bool{"unik rad per id": true}
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
	}
	c := checksOf(t, out)[0].Checks[0]
	if c.Outcome != checksOutcomeAdoptedChanged || !slices.Equal(c.Changed, []string{"expression"}) {
		t.Fatalf("result = %+v", c)
	}
	if got := f.checksOn("table-gl"); len(got) != 1 || got[0].Expression != "count(*) = count(distinct id)" {
		t.Fatalf("checks = %+v", got)
	}
}

// A checks file naming a stem with no .sql is refused per file, and the push
// exits non-zero.
func TestPipelineChecksStemWithoutSQLIsRefused(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if err := wfdir.WriteFile(root, "checks/orders.json", `{"checks":[]}`); err != nil {
		t.Fatal(err)
	}
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("must exit non-zero")
	}
	for _, r := range checksOf(t, out) {
		if r.Path == "checks/orders.json" && (r.Outcome != checksOutcomeRefused || !strings.Contains(r.Error, "no orders.sql")) {
			t.Errorf("result = %+v", r)
		}
	}
}

// An open draft of mine defers the checks even when the live table already
// holds the file's SQL: checks attach to the live row, and the draft is what a
// publish would commit over it.
func TestPipelineChecksOpenDraftDefersEvenWhenLiveMatches(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	f.AddDraft("table-gl", "table-draft-9", glSQL)
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if got := checksOf(t, out)[0]; got.Outcome != checksOutcomePendingPublish || !strings.Contains(got.Error, "open draft") {
		t.Fatalf("result = %+v", got)
	}
	if n := len(f.checkWrites()); n != 0 {
		t.Errorf("checks were written beside an open draft: %d", n)
	}
}

// Clone writes each table's ENABLED checks — no `enabled` key, silenced ones
// left out — records them as owned, and the push after it is a no-op. An edit
// in the app afterwards is drift in `status`.
func TestPipelineChecksCloneThenStatusShowsDrift(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Ekonomi", "private")
	f.AddTable(&api.Table{ID: "table-gl", Name: "gl_lines", FeatureID: "collection-1", Code: glSQL})
	f.AddCheck("table-gl", api.TableCheck{Name: "Tillgångar = eget kapital + skulder", Expression: "abs(sum(amount)) < 1", Severity: "fail", Description: "Balansen.", Enabled: true})
	f.AddCheck("table-gl", api.TableCheck{Name: "Bygger om dagligen", ExpectedIntervalSeconds: 86400, Enabled: true})
	f.AddCheck("table-gl", api.TableCheck{Name: "Gammal vakthund", ExpectedIntervalSeconds: 3600, Enabled: false})
	f.AddCheck("table-gl", api.TableCheck{Name: "Udda intervall", ExpectedIntervalSeconds: 5400, Enabled: true, Kind: api.TableCheckKindFreshness})

	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	} else if !strings.Contains(stderr, "Udda intervall") {
		t.Errorf("a non-whole-hour watchdog must be skipped with a note:\n%s", stderr)
	}
	root := dir + "/out"
	body := readFile(t, root, "checks", "gl_lines.json")
	for _, absent := range []string{`"enabled"`, "Gammal vakthund", "Udda intervall", `\u003c`} {
		if strings.Contains(body, absent) {
			t.Errorf("clone wrote %s:\n%s", absent, body)
		}
	}
	for _, present := range []string{"Tillgångar = eget kapital + skulder", `"expectedIntervalHours": 24`, "abs(sum(amount)) < 1"} {
		if !strings.Contains(body, present) {
			t.Errorf("clone did not write %s:\n%s", present, body)
		}
	}

	before := len(f.checkWrites())
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push after clone: %v\n%s", err, stderr)
	}
	if n := len(f.checkWrites()) - before; n != 0 {
		t.Fatalf("the push after a clone wrote %d time(s)", n)
	}
	if got := checkOutcomes(checksOf(t, out)[0]); got["Bygger om dagligen"] != checksOutcomeUnchanged {
		t.Errorf("outcomes = %v", got)
	}

	f.checkNamed("table-gl", "Tillgångar = eget kapital + skulder").Expression = "true" // edited in the app
	out, _, err = runPipelineCLI(t, root, "pipeline", "status", "--json")
	if err == nil {
		t.Fatal("status must exit non-zero on a check changed in the app")
	}
	var payload struct {
		Remote struct {
			Checks []pipelineChecksStatus `json:"checks"`
		} `json:"remote"`
	}
	decodeJSONInto(t, out, &payload)
	if len(payload.Remote.Checks) != 1 || !slices.Equal(payload.Remote.Checks[0].Drift, []string{"Tillgångar = eget kapital + skulder"}) || payload.Remote.Checks[0].Unmanaged != 2 {
		t.Fatalf("status checks = %+v", payload.Remote.Checks)
	}
}

// ORPHANS ARE DEFINED BY ROW. The app renames an owned check "a" → "B" while the
// file renames its entry "a" → "b": the entry resolves (by folded name) to the
// very row the folder owns, so that row is the entry's target and never an
// orphan — a --prune must not silence it, and the push after must not flip it
// back. Here the rename is drift on both sides, so nothing is written at all.
func TestPipelineChecksOrphanIsByRowNotByName(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"a","expression":"true"}]}`)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	id := f.checkNamed("table-gl", "a").ID
	f.checkNamed("table-gl", "a").Name = "B" // renamed in the app
	writeChecksFile(t, root, `{"checks":[{"name":"b","expression":"true"}]}`)

	before := len(f.checkWrites())
	out, _, _ := runPipelineCLI(t, root, "pipeline", "push", "--prune", "--json")
	for _, w := range f.checkWrites()[before:] {
		if w.CheckID == id && w.Body["enabled"] == false {
			t.Fatalf("the row the file's entry resolves to was pruned as an orphan: %+v\n%s", w, out)
		}
	}
	if got := checksOf(t, out)[0]; got.Outcome == checksOutcomeOrphaned {
		t.Fatalf("an owned row an entry resolves to is not an orphan: %+v", got)
	}
	if !f.checkNamed("table-gl", "B").Enabled {
		t.Fatal("the check was silenced")
	}
	// status reads the same plan: the row is drift, not an orphan.
	out, _, _ = runPipelineCLI(t, root, "pipeline", "status", "--json")
	var payload struct {
		Remote struct {
			Checks []pipelineChecksStatus `json:"checks"`
		} `json:"remote"`
	}
	decodeJSONInto(t, out, &payload)
	if c := payload.Remote.Checks; len(c) != 1 || len(c[0].Orphaned) != 0 || !slices.Equal(c[0].Drift, []string{"b"}) {
		t.Fatalf("status checks = %+v", c)
	}
}

// Two entries that resolve to ONE live row — one by the id the folder owns, one
// by name — refuse the file rather than write the row twice.
func TestPipelineChecksTwoEntriesOnOneRowAreRefused(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"a","expression":"true"}]}`)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	f.checkNamed("table-gl", "a").Name = "b" // renamed in the app
	writeChecksFile(t, root, `{"checks":[{"name":"a","expression":"true"},{"name":"b","expression":"true"}]}`)

	before := len(f.checkWrites())
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("must exit non-zero")
	}
	got := checksOf(t, out)[0]
	if got.Outcome != checksOutcomeRefused || !strings.Contains(got.Error, `"a" and "b"`) {
		t.Fatalf("result = %+v", got)
	}
	if n := len(f.checkWrites()) - before; n != 0 {
		t.Errorf("a refused file wrote %d time(s)", n)
	}
}

// statusChecksOf decodes `pipeline status --json`'s remote checks[].
func statusChecksOf(t *testing.T, root string) []pipelineChecksStatus {
	t.Helper()
	out, _, _ := runPipelineCLI(t, root, "pipeline", "status", "--json")
	var payload struct {
		Remote struct {
			Checks []pipelineChecksStatus `json:"checks"`
		} `json:"remote"`
	}
	decodeJSONInto(t, out, &payload)
	return payload.Remote.Checks
}

// DRIFT-AWARE PRUNE. An orphan an admin changed in the app since the folder last
// verified it is drift when a --prune would silence it: refused, no write, and
// --prune --force silences it. status calls it drift, not orphaned.
func TestPipelineChecksPruneOfAnOrphanChangedInTheAppNeedsForce(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	f.checkNamed("table-gl", "Bygger om dagligen").ExpectedIntervalSeconds = 48 * 3600 // edited in the app
	writeChecksFile(t, root, `{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)","severity":"fail","description":"Dubbletter."}]}`)

	if st := statusChecksOf(t, root); len(st) != 1 || !slices.Equal(st[0].Drift, []string{"Bygger om dagligen"}) || len(st[0].Orphaned) != 0 {
		t.Fatalf("status checks = %+v", st)
	}
	before := len(f.checkWrites())
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--prune", "--json")
	if err == nil {
		t.Fatal("pruning a moved orphan without --force must exit non-zero")
	}
	if got := checksOf(t, out)[0]; got.Outcome != checksOutcomeDrift || !strings.Contains(got.Error, "--prune --force") {
		t.Fatalf("result = %+v", got)
	}
	if n := len(f.checkWrites()) - before; n != 0 {
		t.Fatalf("a refused prune wrote %d time(s)", n)
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--prune", "--force"); err != nil {
		t.Fatalf("push --prune --force: %v\n%s", err, stderr)
	}
	if f.checkNamed("table-gl", "Bygger om dagligen").Enabled {
		t.Error("--prune --force must silence it")
	}
}

// A DELETED CHECKS FILE is a file with no entries: every check it created that
// is still enabled is an orphan — refused, then silenced by --prune — and its
// record is dropped only once nothing it owns is still enabled.
func TestPipelineChecksDeletedFileOrphansItsChecks(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if err := os.Remove(filepath.Join(root, "checks", "gl_lines.json")); err != nil {
		t.Fatal(err)
	}

	if st := statusChecksOf(t, root); len(st) != 1 || !st[0].FileDeleted || len(st[0].Orphaned) != 2 {
		t.Fatalf("status checks = %+v", st)
	}
	before := len(f.checkWrites())
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a deleted file's live checks must refuse the push")
	}
	got := checksOf(t, out)
	if len(got) != 1 || !got[0].FileDeleted || got[0].Outcome != checksOutcomeOrphaned || !strings.Contains(got[0].Error, "Bygger om dagligen") {
		t.Fatalf("result = %+v", got)
	}
	if n := len(f.checkWrites()) - before; n != 0 {
		t.Fatalf("an orphan refusal wrote %d time(s)", n)
	}
	if _, recorded := pipelineStateOf(t, root, f.Key()).Checks["checks/gl_lines.json"]; !recorded {
		t.Fatal("the record was dropped while it still owns enabled checks")
	}

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--prune"); err != nil {
		t.Fatalf("push --prune: %v\n%s", err, stderr)
	}
	for _, row := range f.checksOn("table-gl") {
		if row.Enabled {
			t.Errorf("%q is still enabled", row.Name)
		}
	}
	if _, recorded := pipelineStateOf(t, root, f.Key()).Checks["checks/gl_lines.json"]; recorded {
		t.Error("the record must be dropped once nothing it owns is enabled")
	}
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil || !decodeJSON(t, out)["upToDate"].(bool) {
		t.Fatalf("the push after must be a no-op: %v\n%s\n%s", err, out, stderr)
	}
}

// A STEM RENAME together with its checks file, the table rebound by hand: the
// new file adopts what it declares by name, and the old record's orphans are
// only the checks the new file does NOT declare — a --prune never silences a
// check the new file declares.
func TestPipelineChecksStemRenameHandsTheChecksOver(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	for _, p := range []string{"gl_lines.sql", "checks/gl_lines.json"} {
		if err := os.Remove(filepath.Join(root, p)); err != nil {
			t.Fatal(err)
		}
	}
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Ekonomi"}
	manifest.SetBinding(f.Key(), wfdir.Binding{FeatureID: "collection-1", Tables: map[string]string{"gl.sql": "table-gl"}})
	writePipelineFolder(t, root, manifest, map[string]string{"gl.sql": glSQL,
		"checks/gl.json": `{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)","severity":"fail","description":"Dubbletter."}]}`})
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	inst := state.For(f.Key())
	inst.Files["gl.sql"], inst.Tables["gl.sql"] = inst.Files["gl_lines.sql"], inst.Tables["gl_lines.sql"]
	delete(inst.Files, "gl_lines.sql")
	delete(inst.Tables, "gl_lines.sql")
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("the old file's undeclared check is an orphan and must refuse")
	}
	for _, r := range checksOf(t, out) {
		if r.Path == "checks/gl_lines.json" && (r.Outcome != checksOutcomeOrphaned || strings.Contains(r.Error, "Unik rad per id")) {
			t.Fatalf("only the undeclared check is the old file's orphan: %+v", r)
		}
	}

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--prune"); err != nil {
		t.Fatalf("push --prune: %v\n%s", err, stderr)
	}
	if !f.checkNamed("table-gl", "Unik rad per id").Enabled {
		t.Fatal("a check the new file declares was silenced as the old file's orphan")
	}
	if f.checkNamed("table-gl", "Bygger om dagligen").Enabled {
		t.Error("the undeclared check must be silenced by --prune")
	}
	checks := pipelineStateOf(t, root, f.Key()).Checks
	if _, old := checks["checks/gl_lines.json"]; old || checks["checks/gl.json"].Managed["unik rad per id"].CheckID == "" {
		t.Errorf("records = %+v", checks)
	}
}

// `pipeline publish` whose commit lands and whose checks then do not apply
// exits non-zero, with the commit standing and the checks named.
func TestPipelineChecksPublishExitsNonZeroWhenChecksFailAfterTheCommit(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	writeChecksFile(t, root, oneCheckOfTwo)
	if err := wfdir.WriteFile(root, "gl_lines.sql", "SELECT 2 AS id\n"); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push (checks pending on the draft): %v\n%s", err, stderr)
	}
	out, _, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err == nil {
		t.Fatalf("a publish whose checks did not apply must exit non-zero\n%s", out)
	}
	var cna *checksNotAppliedError
	if !errors.As(err, &cna) {
		t.Errorf("the error must say only the checks failed: %T %v", err, err)
	}
	if f.CodeOf("table-gl") != "SELECT 2 AS id\n" {
		t.Fatalf("the commit did not land: %q", f.CodeOf("table-gl"))
	}
	if got := checksOf(t, out); len(got) != 1 || got[0].Outcome != checksOutcomeOrphaned {
		t.Fatalf("checks = %+v", got)
	}
}

const oneCheckOfTwo = `{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)","severity":"fail","description":"Dubbletter."}]}`

// A TIMED-OUT CREATE THAT LANDED: the re-read finds a row saying exactly what the
// entry says, so it is ours — applied, recorded, with no verdict to report —
// never "adopted", which would claim somebody else made it.
func TestPipelineChecksTimedOutCreateThatLandedIsApplied(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, oneCheckOfTwo)
	landOnceThenTimeOut(t, "POST", "/api/v2/feature/model/table-gl/checks")
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
	}
	c := checksOf(t, out)[0].Checks[0]
	if c.Outcome != checksOutcomeApplied || !strings.Contains(c.Detail, "verdict not returned") || c.CheckID == "" {
		t.Fatalf("result = %+v", c)
	}
	if managedOf(t, root, f.Key())["unik rad per id"].CheckID != c.CheckID {
		t.Errorf("the created check must be recorded as owned: %+v", managedOf(t, root, f.Key()))
	}
	if n := len(f.checksOn("table-gl")); n != 1 {
		t.Errorf("checks on the table = %d", n)
	}
}

// A KIND CHANGE is refused by push, and status says "refused", never "pending".
func TestPipelineChecksKindChangeIsRefused(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"Vakt","expectedIntervalHours":24}]}`)
	f.AddCheck("table-gl", api.TableCheck{Name: "Vakt", Expression: "true", Enabled: true})

	st := statusChecksOf(t, root)
	if len(st) != 1 || !slices.Equal(st[0].KindChange, []string{"Vakt"}) || len(st[0].Pending) != 0 {
		t.Fatalf("status checks = %+v", st)
	}
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a kind change must exit non-zero")
	}
	c := checksOf(t, out)[0].Checks[0]
	if c.Outcome != checksOutcomeRefused || !strings.Contains(c.Error, "kind is fixed") {
		t.Fatalf("result = %+v", c)
	}
	if n := len(f.checkWrites()); n != 0 {
		t.Errorf("a kind change wrote %d time(s)", n)
	}
}

// AN OLDER SERVER'S PUT, which requires `enabled` and changes nothing else: a
// prune (a PUT of {"enabled": false} only) works against it, and an edit is
// `unsupported` for the whole table — never a per-check refusal that reads as a
// bad file.
func TestPipelineChecksOldServerPut(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	f.checkState.oldPut = true

	writeChecksFile(t, root, oneCheckOfTwo)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--prune"); err != nil {
		t.Fatalf("a prune-only push against an older server must succeed: %v\n%s", err, stderr)
	}
	if f.checkNamed("table-gl", "Bygger om dagligen").Enabled {
		t.Fatal("the orphan was not silenced")
	}

	writeChecksFile(t, root, strings.Replace(oneCheckOfTwo, "count(distinct id)", "count(distinct (id))", 1))
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("an edit against an older server must exit non-zero")
	}
	got := checksOf(t, out)[0]
	if got.Outcome != checksOutcomeUnsupported || !strings.Contains(got.Error, "upgrade the server") {
		t.Fatalf("result = %+v", got)
	}

	// A 404 without a Ronja error body is not claimed to be an old server.
	failure := classifyCheckWrite(&api.Error{Status: 404, Body: "<html>Not Found</html>"}, "checks/gl_lines.json")
	if failure.outcome != checksOutcomeUnsupported || !strings.Contains(failure.message, "without a Ronja error body") || !strings.Contains(failure.message, "proxy") {
		t.Errorf("failure = %+v", failure)
	}
}

// A CREATE WITH `enabled: false` is ONE write: the POST carries it, so the
// check never goes live (and never alerts) between a create and a silence. A
// silenced watchdog is created beside the table's enabled one — the server's
// one-watchdog rule binds enabled rows only.
func TestPipelineChecksCreateDisabled(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"Tyst","expression":"true","enabled":false},{"name":"Tyst vakt","expectedIntervalHours":1,"enabled":false}]}`)
	f.AddCheck("table-gl", api.TableCheck{Name: "Levande vakt", ExpectedIntervalSeconds: 86400, Enabled: true, Kind: api.TableCheckKindFreshness})
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	for _, name := range []string{"Tyst", "Tyst vakt"} {
		if row := f.checkNamed("table-gl", name); row == nil || row.Enabled {
			t.Fatalf("%s: row = %+v, want created silenced", name, row)
		}
	}
	writes := f.checkWrites()
	if len(writes) != 2 {
		t.Fatalf("writes = %+v, want exactly two POSTs and no follow-up silence", writes)
	}
	for _, w := range writes {
		if w.Method != "POST" || w.Body["enabled"] != false {
			t.Fatalf("write = %+v, want a POST carrying enabled:false", w)
		}
	}
	if _, recorded := managedOf(t, root, f.Key())["tyst"]; !recorded {
		t.Error("the silenced check was not recorded as the folder's")
	}
}

// A 403 on an edit of a LIVE fail check to an entry that says warn names the
// admin requirement: the server does not say which of the two refused it.
func TestPipelineChecksForbiddenDowngradeNamesAdmin(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, `{"checks":[{"name":"Unik","expression":"true","severity":"warn"}]}`)
	f.AddCheck("table-gl", api.TableCheck{Name: "Unik", Expression: "true", Severity: "fail", Enabled: true})
	f.checkState.nonAdmin = true
	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("must exit non-zero")
	}
	if c := checksOf(t, out)[0].Checks[0]; c.Outcome != checksOutcomeRefused || !strings.Contains(c.Error, "needs admin for a fail check") {
		t.Fatalf("result = %+v", c)
	}
}

// A checks file that cannot be read (a Problem) still OWNS what the lock
// records for it: only its declared names are unavailable, since they cannot
// be parsed. Dropping the recorded ids too let a deleted sibling's record
// orphan — and a --prune silence — a check the broken file owns.
func TestChecksClaimKeepsABrokenFilesOwnedIDs(t *testing.T) {
	inst := &wfdir.InstanceState{Checks: map[string]wfdir.ChecksState{
		"checks/broken.json": {TableID: "table-gl", Managed: map[string]wfdir.LockCheckEntry{"unik": {CheckID: "tchk-owned"}}},
	}}
	f := &folder{Binding: wfdir.Binding{Tables: map[string]string{"broken.sql": "table-gl"}}}
	files := []pipelineChecksFile{{
		Path: "checks/broken.json", SQLPath: "broken.sql", Problem: "invalid JSON",
		File: checkfile.File{Checks: []checkfile.Entry{{Name: "Declared name"}}},
	}}
	claim := checksClaimFor(f, files, f.live(inst), "table-gl", "checks/old.json")
	if !claim.has(&api.TableCheck{ID: "tchk-owned", Name: "Something else"}) {
		t.Fatal("a broken file's recorded check was not claimed — a deleted sibling would orphan it")
	}
	if claim.has(&api.TableCheck{ID: "tchk-other", Name: "Declared name"}) {
		t.Fatal("a broken file's declared names were claimed; they cannot be trusted")
	}
}
