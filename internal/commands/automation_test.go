package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja automation` init / status / push.
//
// Every refusal in this loop is here, and each is paired with the happy path it
// sits beside — a refusal that also refused the ordinary case would be a worse
// bug than the one it prevents, and a test that only asserted the refusal could
// not tell the two apart.

// --- helpers ----------------------------------------------------------------

// anchoredAt is the stamp a folder records when it agrees with a row, in the one
// spelling api.Automation.Stamp writes. Written through the api type on purpose:
// a test that hand-spelled it would pass against a CLI that had changed the
// format, which is exactly the value that must not drift.
func anchoredAt(at time.Time) string {
	row := &api.Automation{UpdatedAt: at}
	return row.Stamp()
}

var (
	baseTime  = time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	laterTime = time.Date(2026, 8, 21, 2, 14, 0, 0, time.UTC)
)

// cronFile is the ordinary automation file the drift and re-enable tests push.
const cronFile = `{
  "triggerKind": "cron",
  "cronExpr": "0 2 * * *",
  "prompt": "Refresh the nightly aggregates."
}`

// --- init -------------------------------------------------------------------

// TestAutomationInitWritesNoBaselineAndNoEnabled: the two things `init` must NOT
// write. `.ronja/` because this is the one folder kind with no local baseline —
// a file nothing ever reads is a file that goes stale and gets believed — and
// `enabled` because a starter file declaring `true` is the very line that
// reverses somebody's pause months later.
func TestAutomationInitWritesNoBaselineAndNoEnabled(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()

	out, _, err := runAutomationCLI(t, dir, "automation", "init",
		"--feature", "collection-1", "--cron", "0 2 * * *", "--workflow", "nightly", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["kind"] != wfdir.KindAutomation {
		t.Errorf("kind = %v", payload["kind"])
	}
	if payload["file"] != "nightly.json" {
		t.Errorf("starter file = %v", payload["file"])
	}
	if _, err := os.Stat(filepath.Join(dir, wfdir.StateDirName)); err == nil {
		t.Errorf("init wrote a .ronja/ baseline; this kind has none")
	}
	starter := readFile(t, dir, "nightly.json")
	if strings.Contains(starter, "enabled") {
		t.Errorf("the starter file declares enabled, which is what reverses a pause:\n%s", starter)
	}
	if !strings.Contains(starter, `"workflowID": "nightly"`) {
		t.Errorf("the starter file does not run the named workflow:\n%s", starter)
	}
	// The alias is DECLARED but not bound: `ronja bind` fills that in, and a
	// manifest that bound it here would be inventing this organization's id.
	manifest := readFile(t, dir, wfdir.ManifestName)
	if !strings.Contains(manifest, `"nightly"`) || !strings.Contains(manifest, `"workflow"`) {
		t.Errorf("the manifest does not declare the workflow dependency:\n%s", manifest)
	}
}

// TestAutomationInitRefusesASecondTime: a second init would overwrite a manifest
// holding real bindings.
func TestAutomationInitRefusesASecondTime(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()

	if _, _, err := runAutomationCLI(t, dir, "automation", "init", "--feature", "collection-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	_, _, err := runAutomationCLI(t, dir, "automation", "init", "--feature", "collection-1")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second init must be refused, got %v", err)
	}
}

// TestAutomationInitRefusesAnUnreadableName: --workflow was validated and --name
// was not, so `--name a/b` wrote a starter file in a subdirectory and named the
// automation after the last segment only, and `--name .x` wrote one no later
// command can see. Refused where the flag is accepted, so the message names the
// flag rather than the path it would have written.
func TestAutomationInitRefusesAnUnreadableName(t *testing.T) {
	for _, name := range []string{"a/b", ".hidden"} {
		f := newFakeAutomationInstance(t)
		signInAutomation(t, f)
		dir := t.TempDir()
		_, _, err := runAutomationCLI(t, dir, "automation", "init",
			"--feature", "collection-1", "--cron", "0 2 * * *", "--name", name)
		if err == nil {
			t.Errorf("--name %q must be refused", name)
			continue
		}
		if !strings.Contains(err.Error(), "--name") {
			t.Errorf("the refusal does not name the flag: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); statErr == nil {
			t.Errorf("--name %q wrote a folder before it was refused", name)
		}
	}
}

// --- the file format --------------------------------------------------------

// TestAutomationFileRefusesTheRowSpellings is the §1.1.3 trap, caught where it
// is cheapest. A file written in the spelling a READ returns would be accepted
// by the server, ignored, and answered 200 — so the folder would report itself
// as pushed for ever while the watched-table set never changed.
func TestAutomationFileRefusesTheRowSpellings(t *testing.T) {
	for key, want := range map[string]string{
		"emailAllowedFromAddrs": "emailAllowedFromAddresses",
		"watchedTableIDs":       "watchedTableIds",
	} {
		_, err := parseAutomationFile("a.json", `{"`+key+`": ["x"]}`)
		if err == nil {
			t.Fatalf("%s was accepted", key)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal for %s does not name the write vocabulary: %v", key, err)
		}
	}
	// And the write vocabulary itself parses.
	if _, err := parseAutomationFile("a.json", `{"watchedTableIds": ["table-1"]}`); err != nil {
		t.Errorf("the input spelling must parse: %v", err)
	}
}

// TestAutomationFileRefusesMarkers: a marker anywhere in the file is refused,
// naming the field. Nothing on the server reads a marker out of an automation,
// so one sent literally is stored as a resource id and fails at the first run.
func TestAutomationFileRefusesMarkers(t *testing.T) {
	body := `{"references": [{"kind": "note", "resourceID": "{{ note('policy') }}"}]}`
	_, err := parseAutomationFile("nightly.json", body)
	if err == nil {
		t.Fatal("a marker in an automation file must be refused")
	}
	if !strings.Contains(err.Error(), "references[0].resourceID") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
	// An invented family is refused exactly as a real one is — which is the
	// point of scanning for `{{ … }}` rather than for the marker table.
	if _, err := parseAutomationFile("a.json", `{"cronExpr": "{{ ref('orders') }}"}`); err == nil {
		t.Error("a real marker family must be refused too")
	}
	// Ordinary content with braces in it is not a marker.
	if _, err := parseAutomationFile("a.json", `{"prompt": "use { and } freely"}`); err != nil {
		t.Errorf("braces that are not a marker must parse: %v", err)
	}
	// The refusal names its own over-reach, because a prompt that legitimately
	// holds a `{{ … }}` is otherwise sent to fix a field nobody wrote.
	_, err = parseAutomationFile("a.json", `{"prompt": "answer with {{ name }}"}`)
	if err == nil {
		t.Fatal("a marker in a prompt is refused too")
	}
	if !strings.Contains(err.Error(), "prompts included") {
		t.Errorf("the refusal does not name its limitation: %v", err)
	}
}

// TestAutomationFileRefusesAnActionWithoutAKind: the server reads a missing
// action kind as the back-compat inline `agent` shape, so an action sent without
// one is accepted, answered 200, and stores something the file never described.
// Refused by name, before the first byte, like every other local guard here.
func TestAutomationFileRefusesAnActionWithoutAKind(t *testing.T) {
	_, err := parseAutomationFile("a.json", `{"action": {"config": {"workflowID": "nightly"}}}`)
	if err == nil {
		t.Fatal("an action with no kind must be refused")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("the refusal does not name the missing key: %v", err)
	}
	if _, err := parseAutomationFile("a.json",
		`{"action": {"kind": "workflow", "config": {"workflowID": "nightly"}}}`); err != nil {
		t.Errorf("an action that names its kind must parse: %v", err)
	}
}

// TestAutomationFileRefusesAPromptOnAPinnedActionKind: a workflow or saved_agent
// automation's parent prompt is pinned to a server-side sentinel — stamped over
// the body at create, and dropped from the patch on every update — so a declared
// value can never converge. Left to the push, the file would report success and
// then drift for ever, with CI exiting 1 over a change that already landed.
func TestAutomationFileRefusesAPromptOnAPinnedActionKind(t *testing.T) {
	for _, kind := range []string{"workflow", "saved_agent"} {
		_, err := parseAutomationFile("a.json",
			`{"prompt": "summarise yesterday", "action": {"kind": "`+kind+`", "config": {}}}`)
		if err == nil {
			t.Fatalf("a prompt beside a %s action must be refused", kind)
		}
		if !strings.Contains(err.Error(), "sentinel") ||
			!strings.Contains(err.Error(), "action.config.prompt") {
			t.Errorf("the refusal for %s does not say why or where to put it instead: %v", kind, err)
		}
	}
	// The inline agent action is whose instructions the top-level prompt IS, so
	// it parses — and that is the shape `automation init` scaffolds.
	if _, err := parseAutomationFile("a.json",
		`{"prompt": "summarise yesterday", "action": {"kind": "agent", "config": {}}}`); err != nil {
		t.Errorf("a prompt beside an agent action must parse: %v", err)
	}
	// The seed message a saved_agent action DOES read is a different field on a
	// different object, and it stays declarable.
	if _, err := parseAutomationFile("a.json",
		`{"action": {"kind": "saved_agent", "config": {"prompt": "summarise yesterday"}}}`); err != nil {
		t.Errorf("action.config.prompt on a saved_agent action must parse: %v", err)
	}
}

// TestAutomationFileRefusesReferencesOnAnActionThatCannotHoldThem: only an
// inline agent action carries references, and the WORKFLOW case is why this is a
// local refusal rather than the server's job.
//
// A saved_agent action carrying references earns a 400. A workflow action
// carrying them is accepted and stores NONE — the create path writes references
// on a branch a workflow action never reaches, and SetWorkflowAction deletes the
// job's reference rows on top — so the push reports success, the row reads back
// empty, and the folder reports drift no edit can close while every later push
// re-sends the same references for ever. That is a permanent false red, and the
// file earns the refusal without a credential.
func TestAutomationFileRefusesReferencesOnAnActionThatCannotHoldThem(t *testing.T) {
	const refs = `"references": [{"kind": "note", "resourceID": "note-1"}]`
	for kind, want := range map[string]string{
		"workflow":    "stores NONE",
		"saved_agent": "OWN references",
	} {
		_, err := parseAutomationFile("a.json",
			`{`+refs+`, "action": {"kind": "`+kind+`", "config": {}}}`)
		if err == nil {
			t.Fatalf("references beside a %s action must be refused", kind)
		}
		if !strings.Contains(err.Error(), "references") || !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal for %s does not say why: %v", kind, err)
		}
	}
	// An inline agent action is where references belong, so it parses.
	if _, err := parseAutomationFile("a.json",
		`{`+refs+`, "action": {"kind": "agent", "config": {}}}`); err != nil {
		t.Errorf("references beside an agent action must parse: %v", err)
	}
	// An EMPTY declaration is allowed on every kind: the server's own gate is
	// len(refs) > 0, and an empty list compares clean against a row that has none
	// — so refusing it would refuse a file that pushes green today.
	for _, kind := range []string{"workflow", "saved_agent"} {
		if _, err := parseAutomationFile("a.json",
			`{"references": [], "action": {"kind": "`+kind+`", "config": {}}}`); err != nil {
			t.Errorf("an empty references list on a %s action must parse: %v", kind, err)
		}
	}

	// And it fires on the push path, before anything is written.
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"nightly.json": `{"cronExpr": "0 2 * * *", ` + refs +
			`, "action": {"kind": "workflow", "config": {"workflowID": "workflow-1"}}}`,
	})
	_, _, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a workflow action carrying references must be refused")
	}
	if len(f.created) != 0 {
		t.Errorf("the refused push created %d automation(s)", len(f.created))
	}
}

// TestAutomationRefusesReferencesAgainstAnActionRowTheFileDoesNotDeclare: the
// SECOND door into the same permanent drift, and the file cannot see it.
//
// A file that declares `references` and NO `action` is unmanaged in the action:
// the push sends none, so the server's `isWorkflowAction` reads a body without
// one, takes the reference branch, and really does write the rows. But the READ
// decodes the config off the stored action JSON, which for a workflow action has
// nowhere to keep references — so the push answers 200, the row comes back
// empty, and status reports drift no edit can ever close. The kind that decides
// is the ROW's, which is why this refusal lives where the row is known.
func TestAutomationRefusesReferencesAgainstAnActionRowTheFileDoesNotDeclare(t *testing.T) {
	// No `action` key at all — the half checkAutomationVocabulary cannot answer.
	const actionless = `{"cronExpr": "0 2 * * *", "references": [{"kind": "note", "resourceID": "note-1"}]}`

	newFolder := func(t *testing.T, kind string) (*fakeAutomationInstance, string) {
		t.Helper()
		f := newFakeAutomationInstance(t)
		signInAutomation(t, f)
		f.AddAutomation(&api.Automation{
			ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true,
			Action: &api.AutomationAction{
				ID: "act-job-1", ScheduledJobID: "job-1", Kind: kind,
				Config: api.AutomationActionConfig{WorkflowID: "workflow-1"},
			},
		})
		dir := t.TempDir()
		manifest, lock := automationStack(f, "collection-1",
			map[string]string{"nightly.json": "job-1"},
			map[string]string{"nightly.json": anchoredAt(baseTime)})
		writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": actionless})
		return f, dir
	}

	f, dir := newFolder(t, api.ActionKindWorkflow)
	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("references against a workflow-action row must be refused")
	}
	if !strings.Contains(stderr, "workflow action") || !strings.Contains(stderr, "stores NONE") {
		t.Errorf("the refusal does not give the rule:\n%s", stderr)
	}
	if len(f.updates) != 0 {
		t.Errorf("the refused push sent %d update(s)", len(f.updates))
	}

	// STATUS SAYS THE SAME THING. A report that called this a pushable change
	// while the push refused it would be this loop's own worst failure, which is
	// why the check sits in the one row-aware refusal both surfaces run.
	// A non-zero exit is the point — an unpushable folder is not a clean one —
	// but the report is still rendered, and the report is what is asserted here.
	out, _, err := runAutomationCLI(t, dir, "automation", "status", "--json")
	if err == nil {
		t.Error("status scored an unpushable folder as clean")
	}
	report := &automationStatusReport{}
	decodeJSONInto(t, out, report)
	if len(report.Remote.Automations) != 1 {
		t.Fatalf("automations = %+v", report.Remote.Automations)
	}
	row := report.Remote.Automations[0]
	if !strings.Contains(row.Problem, "stores NONE") {
		t.Errorf("status did not report the refusal a push would make: %+v", row)
	}
	if len(row.Changes) != 0 {
		t.Errorf("status reported the refused declaration as a pushable change: %+v", row.Changes)
	}

	// A saved_agent row earns its own sentence rather than the workflow one.
	f, dir = newFolder(t, api.ActionKindSavedAgent)
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err == nil {
		t.Fatal("references against a saved_agent-action row must be refused")
	} else if !strings.Contains(stderr, "saved_agent action") {
		t.Errorf("the refusal does not name the row's kind:\n%s", stderr)
	}

	// ⚠️ AND THE ROW IS WHY. The same file against an inline AGENT action row is
	// the ordinary shape and must push — refusing it would refuse every
	// reference-based automation that does not manage its `action`.
	f, dir = newFolder(t, api.ActionKindAgent)
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("references against an agent-action row must push: %v (%s)", err, stderr)
	}
	if refs := f.RowOf("job-1").Action.Config.References; len(refs) != 1 || refs[0].ResourceID != "note-1" {
		t.Errorf("the references did not land on the agent action: %+v", refs)
	}
}

// TestAutomationRefusesAPromptAgainstAnActionRowTheFileDoesNotDeclare is the
// ROW-SIDE half of the prompt rule, the same second door the references rule
// has (refuseAutomationPromptFor is asked from both places, exactly as
// refuseAutomationReferencesFor is).
//
// A file that declares `prompt` and NO `action` is unmanaged in the action: the
// push sends no action, the row keeps the workflow or saved_agent one it has,
// and the server pins that kind's parent prompt to its sentinel. So the push
// answers 200, the row reads back the sentinel, and status reports the same
// drift for ever — the exact permanent false red this rule exists to remove.
// The kind that decides is the ROW's, which is why this leg lives where the row
// is known.
func TestAutomationRefusesAPromptAgainstAnActionRowTheFileDoesNotDeclare(t *testing.T) {
	// No `action` key at all — the half checkAutomationVocabulary cannot answer.
	const actionless = `{"cronExpr": "0 2 * * *", "prompt": "summarise yesterday"}`

	newFolder := func(t *testing.T, kind string) (*fakeAutomationInstance, string) {
		t.Helper()
		f := newFakeAutomationInstance(t)
		signInAutomation(t, f)
		f.AddAutomation(&api.Automation{
			ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true,
			Action: &api.AutomationAction{
				ID: "act-job-1", ScheduledJobID: "job-1", Kind: kind,
				Config: api.AutomationActionConfig{WorkflowID: "workflow-1"},
			},
		})
		dir := t.TempDir()
		manifest, lock := automationStack(f, "collection-1",
			map[string]string{"nightly.json": "job-1"},
			map[string]string{"nightly.json": anchoredAt(baseTime)})
		writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": actionless})
		return f, dir
	}

	f, dir := newFolder(t, api.ActionKindWorkflow)
	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a prompt against a workflow-action row must be refused")
	}
	if !strings.Contains(stderr, "sentinel") || !strings.Contains(stderr, "action.config.prompt") {
		t.Errorf("the refusal does not give the rule or the remedy:\n%s", stderr)
	}
	if len(f.updates) != 0 {
		t.Errorf("the refused push sent %d update(s)", len(f.updates))
	}

	// STATUS SAYS THE SAME THING, for the same reason it does for references: a
	// report that called this a pushable change while the push refused it would
	// be this loop's own worst failure.
	out, _, err := runAutomationCLI(t, dir, "automation", "status", "--json")
	if err == nil {
		t.Error("status scored an unpushable folder as clean")
	}
	report := &automationStatusReport{}
	decodeJSONInto(t, out, report)
	if len(report.Remote.Automations) != 1 {
		t.Fatalf("automations = %+v", report.Remote.Automations)
	}
	if row := report.Remote.Automations[0]; !strings.Contains(row.Problem, "sentinel") {
		t.Errorf("status did not report the refusal a push would make: %+v", row)
	}

	// A saved_agent row is pinned the same way.
	_, dir = newFolder(t, api.ActionKindSavedAgent)
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err == nil {
		t.Fatal("a prompt against a saved_agent-action row must be refused")
	} else if !strings.Contains(stderr, "saved_agent") {
		t.Errorf("the refusal does not name the row's kind:\n%s", stderr)
	}

	// ⚠️ AND THE ROW IS WHY. Against an inline AGENT action row the top-level
	// prompt IS that action's instructions, so it must push — refusing it would
	// refuse the ordinary shape `automation init` scaffolds.
	f, dir = newFolder(t, api.ActionKindAgent)
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("a prompt against an agent-action row must push: %v (%s)", err, stderr)
	}
	if got := f.RowOf("job-1").Prompt; got != "summarise yesterday" {
		t.Errorf("the prompt did not land on the agent action: %q", got)
	}
}

// TestAutomationRefusesSwitchingAnActionBackToAgent: the THIRD door into the
// same permanent drift, by a different field.
//
// PUT :jobID writes a child action row for a workflow or a saved_agent action
// and nothing else — there is no SetAgentAction in the backend — so a file
// declaring kind "agent" against a workflow or saved_agent row pushes green,
// changes nothing, and reports action.kind as drift no edit can close.
//
// The three ALLOWED transitions are asserted beside it, because the way this
// refusal fails is by widening: every other pairing has a route and must push.
func TestAutomationRefusesSwitchingAnActionBackToAgent(t *testing.T) {
	// newFolder stands up one bound folder whose row holds `rowKind` and whose
	// file declares `fileKind`.
	newFolder := func(t *testing.T, rowKind, file string) (*fakeAutomationInstance, string) {
		t.Helper()
		f := newFakeAutomationInstance(t)
		signInAutomation(t, f)
		f.AddAutomation(&api.Automation{
			ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true,
			Action: &api.AutomationAction{
				ID: "act-job-1", ScheduledJobID: "job-1", Kind: rowKind,
				Config: api.AutomationActionConfig{WorkflowID: "workflow-1", AgentID: "agent-1"},
			},
		})
		dir := t.TempDir()
		manifest, lock := automationStack(f, "collection-1",
			map[string]string{"nightly.json": "job-1"},
			map[string]string{"nightly.json": anchoredAt(baseTime)})
		writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": file})
		return f, dir
	}

	const agentFile = `{"cronExpr": "0 2 * * *", "action": {"kind": "agent", "config": {}}}`

	// THE REFUSAL, from both of the kinds that have no way back.
	for _, rowKind := range []string{api.ActionKindWorkflow, api.ActionKindSavedAgent} {
		f, dir := newFolder(t, rowKind, agentFile)
		_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
		if err == nil {
			t.Fatalf("switching a %s action back to agent must be refused", rowKind)
		}
		if !strings.Contains(stderr, rowKind) || !strings.Contains(stderr, "no route back") {
			t.Errorf("the refusal does not give the rule for %s:\n%s", rowKind, stderr)
		}
		if !strings.Contains(stderr, "web app") {
			t.Errorf("the refusal does not name the remedy:\n%s", stderr)
		}
		if len(f.updates) != 0 {
			t.Errorf("the refused push sent %d update(s)", len(f.updates))
		}

		// STATUS SAYS THE SAME THING — the report and the push must never
		// disagree, which is why this sits in the one row-aware refusal both run.
		out, _, err := runAutomationCLI(t, dir, "automation", "status", "--json")
		if err == nil {
			t.Error("status scored an unpushable folder as clean")
		}
		report := &automationStatusReport{}
		decodeJSONInto(t, out, report)
		if len(report.Remote.Automations) != 1 {
			t.Fatalf("automations = %+v", report.Remote.Automations)
		}
		if row := report.Remote.Automations[0]; !strings.Contains(row.Problem, "no route back") {
			t.Errorf("status did not report the refusal a push would make: %+v", row)
		}
	}

	// AND NOT ONE STEP FURTHER. Every other pairing has a setter behind it and
	// must push — over-reaching here would refuse folders that work today.
	const workflowFile = `{"action": {"kind": "workflow", "config": {"workflowID": "workflow-2"}}}`
	const savedAgentFile = `{"action": {"kind": "saved_agent", "config": {"agentID": "agent-2"}}}`
	for _, allowed := range []struct {
		name    string
		rowKind string
		file    string
		want    string
	}{
		// No change at all: the ordinary state of a folder that recorded the
		// inline action it created.
		{"agent to agent", api.ActionKindAgent, agentFile, api.ActionKindAgent},
		// SetSavedAgentAction and SetWorkflowAction each reach the other's kind.
		{"workflow to saved_agent", api.ActionKindWorkflow, savedAgentFile, api.ActionKindSavedAgent},
		{"saved_agent to workflow", api.ActionKindSavedAgent, workflowFile, api.ActionKindWorkflow},
		// Out of agent is the direction that works.
		{"agent to workflow", api.ActionKindAgent, workflowFile, api.ActionKindWorkflow},
		{"agent to saved_agent", api.ActionKindAgent, savedAgentFile, api.ActionKindSavedAgent},
	} {
		t.Run(allowed.name, func(t *testing.T) {
			f, dir := newFolder(t, allowed.rowKind, allowed.file)
			if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
				t.Fatalf("%s must push: %v (%s)", allowed.name, err, stderr)
			}
			if got := f.RowOf("job-1").Action.Kind; got != allowed.want {
				t.Errorf("the row holds a %s action, wanted %s", got, allowed.want)
			}
		})
	}

	// An ABSENT `action` is unmanaged, and a file that declares none against a
	// workflow row is not a switch — refusing it would refuse every folder that
	// leaves the action to the web app.
	f, dir := newFolder(t, api.ActionKindWorkflow, `{"cronExpr": "0 3 * * *"}`)
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("a file managing no action must push: %v (%s)", err, stderr)
	}
	if got := f.RowOf("job-1").Action.Kind; got != api.ActionKindWorkflow {
		t.Errorf("an unmanaged action changed kind to %s", got)
	}
}

// TestAutomationCreateTakesAnAgentActionKind: a CREATE has no row to compare
// against and the create route stores whatever kind it is given, so the refusal
// must not reach it.
func TestAutomationCreateTakesAnAgentActionKind(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"nightly.json": `{"cronExpr": "0 2 * * *", "action": {"kind": "agent", "config": {}}}`,
	})
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("creating an inline agent automation must push: %v (%s)", err, stderr)
	}
	if len(f.created) != 1 {
		t.Fatalf("the push created %d automation(s)", len(f.created))
	}
}

// TestAutomationFileRefusesUnknownKeys: an unknown key is the silent-drop class
// this whole loop exists to remove — the file says one thing, the row keeps
// another, and nothing reports a difference.
func TestAutomationFileRefusesUnknownKeys(t *testing.T) {
	if _, err := parseAutomationFile("a.json", `{"cronExpression": "0 2 * * *"}`); err == nil {
		t.Error("an unknown key must be refused rather than dropped")
	}
	for _, key := range []string{"name", "private", "id", "featureID", "webhookSigningSecret"} {
		_, err := parseAutomationFile("a.json", `{"`+key+`": "x"}`)
		if err == nil {
			t.Errorf("%q must be refused with its own reason", key)
		}
	}
}

// TestAutomationDeltaIgnoresUnmanagedFields: a field a file does not mention is
// not managed, so it is neither reported as a change nor patched. `enabled` is
// the field this exists for — the three-state pointer is what stops a folder
// silently reversing a pause — but it holds for every field.
func TestAutomationDeltaIgnoresUnmanagedFields(t *testing.T) {
	file, err := parseAutomationFile("nightly.json", `{"cronExpr": "0 2 * * *"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	row := &api.Automation{
		Name: "nightly", CronExpr: "0 2 * * *", Enabled: false, DisabledReason: "user",
		Model: "opus", Description: "set in the web app",
	}
	row.Normalize()
	changed, patch := automationDelta("nightly", file, row)
	if len(changed) != 0 {
		t.Errorf("undeclared fields were reported as changes: %v", changed)
	}
	if !patch.Empty() {
		t.Errorf("undeclared fields produced a patch: %+v", patch)
	}
}

// TestAutomationDeltaCrossesTheSpellingGap: the file's write-vocabulary field is
// compared against the ROW's read-vocabulary one. Comparing like-named fields
// would report every email and table trigger as permanently drifted, with
// nothing the author could edit to fix it.
func TestAutomationDeltaCrossesTheSpellingGap(t *testing.T) {
	file, err := parseAutomationFile("t.json",
		`{"watchedTableIds": ["table-1"], "emailAllowedFromAddresses": ["a@example.com"]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	row := &api.Automation{
		Name:                  "t",
		WatchedTableIDs:       []string{"table-1"},
		EmailAllowedFromAddrs: []string{"a@example.com"},
	}
	row.Normalize()
	if changed, _ := automationDelta("t", file, row); len(changed) != 0 {
		t.Errorf("a matching declaration read as drift across the spelling gap: %v", changed)
	}
}

// TestAutomationPushKeepsAnUnmanagedActionConfigField is the nested half of the
// three-state model, and the one place "absent is unmanaged" is hard to keep.
//
// The server REPLACES an action's config wholesale — writeActionTx deletes the
// action row and inserts a new one from the config the body carried — so a push
// that sends the action at all decides the value of every config field, not just
// the one it meant to change. A file that manages only `workflowID` therefore has
// to carry the row's own `parameterValues` forward, or changing the workflow
// silently wipes the parameter values somebody set in the web app: not listed in
// `changed`, not reported by the post-write verification, and indistinguishable
// a day later from somebody clearing them by hand.
func TestAutomationPushKeepsAnUnmanagedActionConfigField(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	row := f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", Enabled: true})
	row.Action = &api.AutomationAction{
		ID: "act-job-1", ScheduledJobID: "job-1", Kind: api.ActionKindWorkflow,
		Config: api.AutomationActionConfig{
			WorkflowID:      "workflow-old",
			ParameterValues: map[string]any{"since": "2026-01-01"},
		},
	}
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"nightly.json": `{"action": {"kind": "workflow", "config": {"workflowID": "workflow-new"}}}`,
	})

	stdout, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err != nil {
		t.Fatalf("push: %v (%s)", err, stderr)
	}
	stored := f.RowOf("job-1")
	if stored.Action.Config.WorkflowID != "workflow-new" {
		t.Errorf("the managed field did not land: %+v", stored.Action.Config)
	}
	if got := stored.Action.Config.ParameterValues["since"]; got != "2026-01-01" {
		t.Errorf("an UNMANAGED action config field was cleared by a push that changed a different one: %+v",
			stored.Action.Config.ParameterValues)
	}
	if strings.Contains(stdout, "parameterValues") {
		t.Errorf("a field this push never managed was reported as written:\n%s", stdout)
	}
}

// TestAutomationDeltaIgnoresAnUnmanagedActionConfigField is the same rule at the
// comparison: an action config field a file does not carry is not drift either,
// so `status` does not report a change a push would not make.
func TestAutomationDeltaIgnoresAnUnmanagedActionConfigField(t *testing.T) {
	file, err := parseAutomationFile("nightly.json",
		`{"action": {"kind": "saved_agent", "config": {"agentID": "agent-1"}}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	row := &api.Automation{
		Name: "nightly",
		Action: &api.AutomationAction{
			Kind: api.ActionKindSavedAgent,
			Config: api.AutomationActionConfig{
				AgentID: "agent-1", Prompt: "seeded in the web app",
			},
		},
	}
	row.Normalize()
	changed, patch := automationDelta("nightly", file, row)
	if len(changed) != 0 {
		t.Errorf("an undeclared action config field read as drift: %v", changed)
	}
	if !patch.Empty() {
		t.Errorf("an undeclared action config field produced a patch: %+v", patch)
	}
}

// --- refusal 1: the re-enable gate ------------------------------------------

// TestAutomationPushRefusesReversingAPause is the 02:00 scenario: an operator
// pauses a runaway automation, and CI pushes a folder whose file says
// enabled:true on the next merge. The pause is CLEARABLE server-side, so without
// this gate the automation is live again with nobody in the loop.
//
// The message is what this gate earns — the ordinary drift refusal already
// covers the same rows — so the test asserts the message names the pause.
func TestAutomationPushRefusesReversingAPause(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"nightly.json": `{"cronExpr": "0 2 * * *", "enabled": true}`,
	})

	// Before the pause the same folder pushes cleanly: the file agrees with the
	// row, so there is nothing to write and nothing to refuse.
	if _, _, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("the happy path must push: %v", err)
	}

	f.Pause("job-1", "user", laterTime)
	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a false→true flip against a paused row must be refused")
	}
	if !strings.Contains(stderr, "disabled it at") || !strings.Contains(stderr, "a person") {
		t.Errorf("the refusal does not name who paused it and when:\n%s", stderr)
	}
	if row := f.RowOf("job-1"); row.Enabled {
		t.Error("the refused push turned the automation back on")
	}
	// --force is the explicit way through, and it says so.
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push", "--force"); err != nil {
		t.Fatalf("--force must push: %v (%s)", err, stderr)
	}
	if row := f.RowOf("job-1"); !row.Enabled {
		t.Error("--force did not re-enable the automation")
	}
}

// TestAutomationReEnableGateIsDisarmedWithoutAnAnchor: a folder that never
// recorded when it agreed with the row cannot say the pause came afterwards, and
// claiming otherwise would refuse a first push against a legitimately disabled
// automation.
func TestAutomationReEnableGateIsDisarmedWithoutAnAnchor(t *testing.T) {
	file, err := parseAutomationFile("a.json", `{"enabled": true}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	row := &api.Automation{ID: "job-1", Enabled: false, DisabledReason: "user", UpdatedAt: laterTime}
	if err := automationReEnableGuard(file, row, ""); err != nil {
		t.Errorf("an unanchored folder must not be refused: %v", err)
	}
	if err := automationReEnableGuard(file, row, anchoredAt(baseTime)); err == nil {
		t.Error("an anchored folder must be refused")
	}
	// The anchor is compared as an INSTANT. RFC3339Nano trims trailing zeros, so
	// `2026-08-10T10:00:00Z` and a fractional spelling of the same moment are the
	// same instant and a string comparison would order them wrongly.
	sameMoment := "2026-08-10T10:00:00.000Z"
	if err := automationReEnableGuard(file, &api.Automation{Enabled: false, UpdatedAt: baseTime}, sameMoment); err != nil {
		t.Errorf("a row that has not moved must not be refused: %v", err)
	}
}

// --- refusal 2: the row moved -----------------------------------------------

// TestAutomationPushRefusesAMovedRow: this loop has no version and no
// compare-and-set, so the recorded updatedAt is the ONLY drift guard there is. A
// push past it would overwrite whatever somebody changed in the web app without
// knowing what it was.
func TestAutomationPushRefusesAMovedRow(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 6 * * *", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	// Anchored and unmoved: the ordinary edit pushes.
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("an anchored push must land: %v (%s)", err, stderr)
	}
	if row := f.RowOf("job-1"); row.CronExpr != "0 2 * * *" {
		t.Fatalf("the push did not write the schedule: %q", row.CronExpr)
	}
	// The anchor moved with it, so a second push is a no-op rather than a
	// self-inflicted drift refusal.
	if _, _, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("a second push must be clean: %v", err)
	}

	// Somebody edits it in the web app.
	f.Touch("job-1", laterTime)
	if err := os.WriteFile(filepath.Join(dir, "nightly.json"),
		[]byte(`{"cronExpr": "0 3 * * *"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a push against a moved row must be refused")
	}
	if !strings.Contains(stderr, "changed on the server") || !strings.Contains(stderr, "status") {
		t.Errorf("the refusal does not point at status:\n%s", stderr)
	}
	if row := f.RowOf("job-1"); row.CronExpr != "0 2 * * *" {
		t.Errorf("the refused push wrote anyway: %q", row.CronExpr)
	}
	if _, _, err := runAutomationCLI(t, dir, "automation", "push", "--force"); err != nil {
		t.Fatalf("--force must push: %v", err)
	}
	if row := f.RowOf("job-1"); row.CronExpr != "0 3 * * *" {
		t.Errorf("--force did not write: %q", row.CronExpr)
	}
}

// TestAutomationLegacyFolderWritesNoAnchor: a legacy instances[] folder has no
// stack name, so the drift guard is DISARMED for it and says so — the one thing
// it must not do is half-arm itself.
//
// The unchanged branch of a push wrote the anchor unconditionally, so such a
// folder committed `"stacks":{"":{…}}` into ronja.lock.json; nothing ever
// refreshed that entry, and every later push then refused "this changed on the
// server" for ever, --force included. A folder made permanently unpushable by a
// key nobody typed.
func TestAutomationLegacyFolderWritesNoAnchor(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{
		ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *",
		Prompt: "Refresh the nightly aggregates.", Enabled: true,
	})
	dir := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindAutomation, Title: "Orders automations",
		Instances: []wfdir.Instance{{
			URL: f.URL(), TenantID: testTenantID,
			Binding: wfdir.Binding{
				FeatureID:   "collection-1",
				Automations: map[string]string{"nightly.json": "job-1"},
			},
		}},
	}
	writeAutomationFolder(t, dir, manifest, nil, map[string]string{"nightly.json": cronFile})

	// The file already matches: the unchanged branch, which is the one that wrote.
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("an unchanged legacy folder must push: %v (%s)", err, stderr)
	}
	lock, err := wfdir.LoadLock(dir)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	if _, wrote := lock.Stacks[""]; wrote {
		t.Errorf("an unnamed stack was committed to the lock: %v", lock.Stacks)
	}

	// Somebody edits it in the web app. The guard is off for this folder shape,
	// so the push lands — what it must never do is refuse against an anchor it
	// had no business recording and cannot refresh.
	f.Touch("job-1", laterTime)
	if err := os.WriteFile(filepath.Join(dir, "nightly.json"),
		[]byte(`{"cronExpr": "0 3 * * *"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("a legacy folder must not be refused against an anchor it cannot keep: %v (%s)", err, stderr)
	}
	if row := f.RowOf("job-1"); row.CronExpr != "0 3 * * *" {
		t.Errorf("the push did not write: %q", row.CronExpr)
	}
}

// --- refusal 3: the unpatchable fields --------------------------------------

// TestAutomationPushRefusesUnpatchableFields covers all THREE fields the update
// body cannot carry, not just the trigger kind: sending any of them would answer
// 200 for a write that never happened.
func TestAutomationPushRefusesUnpatchableFields(t *testing.T) {
	cases := map[string]struct {
		file string
		want string
	}{
		"triggerKind":    {`{"triggerKind": "webhook"}`, "triggerKind"},
		"referenceBased": {`{"referenceBased": false}`, "referenceBased"},
		"eventName":      {`{"eventName": "invoice.settled"}`, "eventName"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeAutomationInstance(t)
			signInAutomation(t, f)
			f.AddAutomation(&api.Automation{
				ID: "job-1", Name: "nightly", TriggerKind: api.TriggerKindCron,
				ReferenceBased: true, Enabled: true,
			})
			dir := t.TempDir()
			manifest, lock := automationStack(f, "collection-1",
				map[string]string{"nightly.json": "job-1"},
				map[string]string{"nightly.json": anchoredAt(baseTime)})
			writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": tc.file})

			_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
			if err == nil {
				t.Fatalf("changing %s must be refused", name)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("the refusal does not name the field:\n%s", stderr)
			}
			// The remedy is the web app, and never delete-and-recreate: a delete
			// is a soft delete this CLI cannot empty.
			if !strings.Contains(stderr, "web app") {
				t.Errorf("the refusal does not name the remedy:\n%s", stderr)
			}
			if len(f.updates) != 0 {
				t.Errorf("the refused push sent %d update(s)", len(f.updates))
			}
			// Declaring the value the row already HAS is not a change, and pushes.
			echo := strings.NewReplacer("webhook", "cron", "false", "true",
				`"eventName": "invoice.settled"`, `"cronExpr": "0 2 * * *"`).Replace(tc.file)
			if err := os.WriteFile(filepath.Join(dir, "nightly.json"), []byte(echo), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
				t.Fatalf("echoing the row's own value must push: %v (%s)", err, stderr)
			}
		})
	}
}

// --- refusal 4: a deleted file ----------------------------------------------

// TestAutomationPushRefusesADeletedFile: a file lost to a bad rebase must not
// silently stop a live automation. --prune is the explicit opt-in, and it says
// what a delete actually is.
func TestAutomationPushRefusesADeletedFile(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	f.AddAutomation(&api.Automation{ID: "job-2", Name: "invoices", CronExpr: "0 5 * * *", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1", "invoices.json": "job-2"},
		map[string]string{
			"nightly.json":  anchoredAt(baseTime),
			"invoices.json": anchoredAt(baseTime),
		})
	// invoices.json is BOUND and not on disk — the rebase that dropped it.
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a bound file that is gone must refuse the push")
	}
	if !strings.Contains(err.Error()+stderr, "invoices.json") {
		t.Errorf("the refusal does not name the orphan: %v\n%s", err, stderr)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("the refused push deleted %v", f.deleted)
	}
	if f.RowOf("job-2") == nil {
		t.Fatal("the automation behind the deleted file is gone")
	}
	// And it refused BEFORE writing anything: a folder-wide guard, not a per-file
	// one, so the surviving file was not pushed either.
	if len(f.updates) != 0 || len(f.created) != 0 {
		t.Errorf("the refused push wrote: %d update(s), %d create(s)", len(f.updates), len(f.created))
	}

	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push", "--prune"); err != nil {
		t.Fatalf("--prune must push: %v (%s)", err, stderr)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "job-2" {
		t.Errorf("--prune deleted %v", f.deleted)
	}
	// The binding goes with it, so the next push does not report it again.
	binding := automationBindingOf(t, dir, f.Key())
	if _, still := binding.Automations["invoices.json"]; still {
		t.Error("the pruned path is still bound")
	}
}

// TestAutomationPruneRefusesAMovedRow: --prune obeys the drift anchor, and the
// argument is stronger than it is for an update. Every write in this loop refuses
// a row that moved since the folder last agreed with it; a DELETE is the most
// expensive of them, and "a file went missing from a rebase" is the weakest
// reason any of them has. So the pairing that costs somebody their work — a lost
// file plus a row they have been editing in the web app — is exactly the one
// that must not go through unattended.
func TestAutomationPruneRefusesAMovedRow(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	f.AddAutomation(&api.Automation{ID: "job-2", Name: "invoices", CronExpr: "0 5 * * *", Enabled: true})
	// Somebody has been working on the one the rebase dropped the file for.
	f.Touch("job-2", laterTime)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1", "invoices.json": "job-2"},
		map[string]string{
			"nightly.json":  anchoredAt(baseTime),
			"invoices.json": anchoredAt(baseTime),
		})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	_, stderr, err := runAutomationCLI(t, dir, "automation", "push", "--prune")
	if err == nil {
		t.Fatal("--prune against a moved row must be refused")
	}
	if !strings.Contains(stderr, "changed on the server") || !strings.Contains(stderr, "invoices.json") {
		t.Errorf("the refusal does not name the row or the reason:\n%s", stderr)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("the refused prune deleted %v", f.deleted)
	}
	// The binding survives the refusal, or the next push would create a second
	// automation beside the live one.
	if _, still := automationBindingOf(t, dir, f.Key()).Automations["invoices.json"]; !still {
		t.Error("a refused prune dropped the binding")
	}

	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push", "--prune", "--force"); err != nil {
		t.Fatalf("--prune --force must delete: %v (%s)", err, stderr)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "job-2" {
		t.Errorf("--force deleted %v", f.deleted)
	}
}

// --- refusal 5: mailbox authority -------------------------------------------

// TestAutomationPushRefusesAMailboxWithoutAdmin: binding a mailbox needs the
// admin role and an admin-scoped token, while this loop's own scope is
// `automation`. Said before sending, so the refusal does not arrive as a 403 on
// the fourth of nine files with the first three already written.
func TestAutomationPushRefusesAMailboxWithoutAdmin(t *testing.T) {
	const mailboxFile = `{
  "triggerKind": "mailbox",
  "mailboxID": "mailbox-abc",
  "action": {"kind": "saved_agent", "config": {"agentID": "agent-1"}}
}`
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"inbox.json": mailboxFile})

	_, _, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a non-admin must be refused before the push")
	}
	if !strings.Contains(err.Error(), "admin") || !strings.Contains(err.Error(), "inbox.json") {
		t.Errorf("the refusal does not name the role or the file: %v", err)
	}
	if len(f.created) != 0 {
		t.Errorf("the refused push created %d automation(s)", len(f.created))
	}

	// An admin gets through, and is told about the half nothing can check: no
	// route reports what SCOPES a token was minted with.
	f.privilegeLevel = adminPrivilegeLevel
	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err != nil {
		t.Fatalf("an admin must push: %v (%s)", err, stderr)
	}
	if !strings.Contains(stderr, "admin:write") {
		t.Errorf("an admin is not told about the scope nothing can verify:\n%s", stderr)
	}
	if len(f.created) != 1 {
		t.Fatalf("the admin push created %d automation(s)", len(f.created))
	}
}

// --- refusal 6: a truncated listing -----------------------------------------

// TestAutomationTruncatedListingIsUnknownNotEmpty is the sharpest false green on
// this surface. A listing that saw a prefix of the feature cannot say which rows
// are missing; read as "no automations" it makes `status` report every binding
// as broken and makes `push` create duplicates beside live rows.
func TestAutomationTruncatedListingIsUnknownNotEmpty(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	// The feature holds far more than one page carries.
	f.totalOverride = 500
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	out, _, err := runAutomationCLI(t, dir, "automation", "status", "--json")
	if err == nil {
		t.Fatal("a truncated listing must not exit zero")
	}
	payload := decodeJSON(t, out)
	remote, _ := payload["remote"].(map[string]any)
	if remote == nil || remote["problem"] == nil {
		t.Fatalf("the truncation is not reported as a folder problem: %v", payload["remote"])
	}
	if remote["automations"] != nil {
		t.Errorf("a truncated listing reported per-row answers: %v", remote["automations"])
	}
	// And the tree verdict is `unknown`, never `drifted` and never `clean`.
	report := &automationStatusReport{}
	decodeJSONInto(t, out, report)
	if v := verdictOfAutomationStatus(report, 1); v.Verdict != verdictUnknown {
		t.Errorf("verdict = %q, want unknown (%s)", v.Verdict, v.Detail)
	}

	// A push refuses outright rather than creating a second automation.
	_, _, pushErr := runAutomationCLI(t, dir, "automation", "push")
	if pushErr == nil {
		t.Fatal("a push against a truncated listing must be refused")
	}
	if len(f.created) != 0 {
		t.Errorf("the refused push created %d automation(s)", len(f.created))
	}
}

// TestAutomationListDropsAnotherFeaturesRows: an OLDER backend accepts
// `featureID`, ignores it, and answers with everything the caller can see. Those
// rows must not be read as this feature's — the orphan and unmanaged legs act on
// exactly that set.
func TestAutomationListDropsAnotherFeaturesRows(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.ignoreFeatureFilter = true
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	f.AddAutomation(&api.Automation{ID: "job-9", Name: "elsewhere", FeatureID: "collection-other", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	out, _, _ := runAutomationCLI(t, dir, "automation", "status", "--json")
	report := &automationStatusReport{}
	decodeJSONInto(t, out, report)
	for _, name := range report.Remote.Unmanaged {
		if strings.Contains(name, "job-9") {
			t.Errorf("another feature's automation was reported as this feature's: %v", report.Remote.Unmanaged)
		}
	}
}

// --- refusal: field-wise alias resolution -----------------------------------

// TestAutomationPushResolvesAliasesByField: the aliases resolve per FIELD, and an
// unbound one is refused by name. A textual pass could not do either — `note`
// has no marker family at all.
//
// ⚠️ TWO FILES, because the two fields cannot share one. References belong to an
// inline agent action and nothing else (checkAutomationVocabulary refuses them
// beside a workflow action, which stores none), so proving field-wise resolution
// across BOTH an ordinary field and one nested inside `action.config` takes an
// agent automation and a workflow automation in the same folder. The `note` half
// is the load-bearing one: it is the kind with no marker family, reachable only
// as a structured field.
func TestAutomationPushResolvesAliasesByField(t *testing.T) {
	const withRefs = `{
  "cronExpr": "0 3 * * *",
  "references": [{"kind": "note", "resourceID": "policy"}]
}`
	const withWorkflow = `{
  "cronExpr": "0 2 * * *",
  "action": {"kind": "workflow", "config": {"workflowID": "nightly"}}
}`
	// ⚠️ Neither stem may equal a dependency alias — the folder refuses a file
	// and an alias that share a name, since the file would win and the
	// declaration would never resolve anything.
	files := map[string]string{"policy-watch.json": withRefs, "nightly-run.json": withWorkflow}
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	manifest.Dependencies = map[string]wfdir.Dependency{
		"policy":  {Kind: "note"},
		"nightly": {Kind: "workflow"},
	}
	writeAutomationFolder(t, dir, manifest, lock, files)

	// Declared and NOT bound: refused, with no request made.
	_, _, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("an alias with no bind must be refused")
	}
	if len(f.created) != 0 {
		t.Errorf("the refused push created %d automation(s)", len(f.created))
	}

	// Bound: each field is resolved to its own kind's id.
	stack := manifest.Stacks["prod"]
	stack.Bind = map[string]string{"policy": "note-1", "nightly": "workflow-1"}
	manifest.Stacks["prod"] = stack
	writeAutomationFolder(t, dir, manifest, lock, files)

	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("a bound folder must push: %v (%s)", err, stderr)
	}
	if len(f.created) != 2 {
		t.Fatalf("created %d automation(s)", len(f.created))
	}
	created := map[string]api.CreateAutomationInput{}
	for _, in := range f.created {
		created[in.Name] = in
	}
	if action := created["nightly-run"].Action; action == nil || action.Config.WorkflowID != "workflow-1" {
		t.Errorf("action.config.workflowID was not resolved: %+v", action)
	}
	if refs := created["policy-watch"].References; len(refs) != 1 || refs[0].ResourceID != "note-1" {
		t.Errorf("references[0].resourceID was not resolved: %+v", refs)
	}
	// A second push is clean: the ids the server holds de-resolve to the same
	// declaration, so an aliased folder does not read as permanently drifted.
	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("a second push must be clean: %v (%s)", err, stderr)
	}
	if len(f.updates) != 0 {
		t.Errorf("an unchanged aliased folder re-pushed: %+v", f.updates)
	}
}

// TestAutomationPushRefusesAnUnaliasableReferenceKind: `dataapp`, `mcpserver`
// and `feature` have no alias, so naming one means committing a raw id — a
// folder that deploys green to the second stack and names an id nobody there
// has.
func TestAutomationPushRefusesAnUnaliasableReferenceKind(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"run.json": `{"cronExpr": "0 2 * * *", "references": [{"kind": "dataapp", "resourceID": "dataapp-1"}]}`,
	})

	_, _, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a reference kind with no alias must be refused")
	}
	if !strings.Contains(err.Error(), "references[0].resourceID") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
	// An unknown kind is refused too, at parse time, rather than sent.
	if _, perr := parseAutomationFile("a.json",
		`{"references": [{"kind": "banana", "resourceID": "x"}]}`); perr == nil {
		t.Error("an unknown reference kind must be refused")
	}
}

// --- the half-applied update ------------------------------------------------

// TestAutomationPushReportsAHalfAppliedUpdate: the row update and the reference
// write are SEPARATE server-side calls, so a failure at the second leaves the
// new schedule with the old references. Reported at the moment it happens, since
// a day later it is indistinguishable from somebody editing in the web app.
func TestAutomationPushReportsAHalfAppliedUpdate(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	row := f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 6 * * *", Enabled: true})
	row.Action.Config.References = []api.AutomationReference{{Kind: "note", ResourceID: "note-old"}}
	f.halfApply["job-1"] = true
	f.failUpdate["job-1"] = 500
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"nightly.json": `{"cronExpr": "0 2 * * *", "references": [{"kind": "note", "resourceID": "note-new"}]}`,
	})

	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a failed update must not report success")
	}
	if !strings.Contains(stderr, "HALF-APPLIED") {
		t.Errorf("the half-apply was not reported at the moment it happened:\n%s", stderr)
	}
	if !strings.Contains(stderr, "cronExpr") || !strings.Contains(stderr, "references") {
		t.Errorf("the report does not say which half landed:\n%s", stderr)
	}
}

// --- status and the tree verdict --------------------------------------------

// TestAutomationNeverDeployedIsDriftedNotClean: a folder holding files that have
// never been pushed is NOT a healthy repository, and reporting it as one is the
// false green that survived two loops before this rule was written down.
func TestAutomationNeverDeployedIsDriftedNotClean(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	out, _, _ := runAutomationCLI(t, dir, "automation", "status", "--json")
	report := &automationStatusReport{}
	decodeJSONInto(t, out, report)
	if report.Remote.NotCheckedReason != reasonNoAutomationsYet {
		t.Fatalf("reason = %q", report.Remote.NotCheckedReason)
	}
	if v := verdictOfAutomationStatus(report, 1); v.Verdict != verdictDrifted {
		t.Errorf("a folder holding an undeployed automation is %q, want drifted", v.Verdict)
	}
	// An EMPTY folder genuinely has nothing to deploy, and is clean.
	if v := verdictOfAutomationStatus(report, 0); v.Verdict != verdictClean {
		t.Errorf("an empty folder is %q, want clean", v.Verdict)
	}
}

// TestAutomationOrphanIsDriftedInTheTree: an orphan is neither undeployed work
// nor a change on the server — a push REFUSES on it — so it must move the
// verdict without being described as either.
func TestAutomationOrphanIsDriftedInTheTree(t *testing.T) {
	report := &automationStatusReport{
		Orphans: []string{"invoices.json"},
		Remote:  &automationRemoteReport{Checked: true},
	}
	v := verdictOfAutomationStatus(report, 1)
	if v.Verdict != verdictDrifted {
		t.Fatalf("verdict = %q, want drifted", v.Verdict)
	}
	if !strings.Contains(v.Detail, "invoices.json") || !strings.Contains(v.Detail, "refuses") {
		t.Errorf("detail = %q", v.Detail)
	}
	if strings.Contains(v.Detail, "never been deployed") {
		t.Errorf("an orphan was described as undeployed work: %q", v.Detail)
	}
}

// TestAutomationStatusReportsChangesWithoutWriting: `status` says what a push
// would change, and its answer is computed by the same function the push writes
// from — so the two cannot disagree.
func TestAutomationStatusReportsChangesWithoutWriting(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 6 * * *", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	out, _, err := runAutomationCLI(t, dir, "automation", "status", "--json")
	if err != nil {
		t.Fatalf("a local change is not the gate: %v", err)
	}
	report := &automationStatusReport{}
	decodeJSONInto(t, out, report)
	if len(report.Remote.Automations) != 1 {
		t.Fatalf("automations = %+v", report.Remote.Automations)
	}
	row := report.Remote.Automations[0]
	if len(row.Changes) == 0 || !strings.Contains(strings.Join(row.Changes, ","), "cronExpr") {
		t.Errorf("status did not report the change a push would make: %+v", row.Changes)
	}
	if row.Drift != driftNone {
		t.Errorf("drift = %q, want none", row.Drift)
	}
	// Read-only: no write of any kind.
	if n := f.requestsMatching("POST ") + f.requestsMatching("PUT ") + f.requestsMatching("DELETE "); n != 0 {
		t.Errorf("status made %d write request(s): %v", n, f.requestOrder())
	}
}

// TestAutomationPushVerifiesWhatTheServerStored: a field the server accepts and
// does not store answers 200 and changes nothing — the class of bug the two
// spelling mismatches on this surface produce. The write is checked against the
// row it returned rather than assumed.
func TestAutomationPushVerifiesWhatTheServerStored(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"nightly.json": `{"cronExpr": "0 2 * * *", "watchedTableIds": ["table-7"]}`,
	})

	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("push: %v (%s)", err, stderr)
	}
	// The fake stores the INPUT spelling and emits the ROW spelling, exactly as
	// production does. A CLI that sent the row's name would land nothing here.
	if row := f.RowOf("job-1"); len(row.WatchedTableIDs) != 1 || row.WatchedTableIDs[0] != "table-7" {
		t.Errorf("the watched table did not land: %+v", row.WatchedTableIDs)
	}
	// And the anchor moved with the write, so the next push is not a
	// self-inflicted drift refusal.
	if seen := automationSeenOf(t, dir, "prod", "nightly.json"); seen == anchoredAt(baseTime) || seen == "" {
		t.Errorf("the anchor was not re-recorded after the write: %q", seen)
	}
}

// TestAutomationPushSendsAnEmptyListRatherThanNull: clearing the last entry of a
// list means sending `[]`. A pointer to a NIL slice marshals as `null`, which
// the server reads as "absent" — so the clear would silently keep the list.
func TestAutomationPushSendsAnEmptyListRatherThanNull(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{
		ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true,
		WatchedTableIDs: []string{"table-7"},
	})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"nightly.json": `{"cronExpr": "0 2 * * *", "watchedTableIds": []}`,
	})

	if _, stderr, err := runAutomationCLI(t, dir, "automation", "push"); err != nil {
		t.Fatalf("push: %v (%s)", err, stderr)
	}
	if len(f.updates) != 1 {
		t.Fatalf("updates = %+v", f.updates)
	}
	if got := string(f.updates[0].Body["watchedTableIds"]); got != "[]" {
		t.Errorf("watchedTableIds was sent as %s, want []", got)
	}
	if row := f.RowOf("job-1"); len(row.WatchedTableIDs) != 0 {
		t.Errorf("the list was not cleared: %+v", row.WatchedTableIDs)
	}
}

// TestAutomationStatusReportsAnOrphanAsAnOrphan: the row behind a deleted file
// reads perfectly — what is missing is the file. Reported as a per-row PROBLEM it
// would score as "could not be checked", which sends a reader looking for an
// instance that would not answer and hides the one sentence that says what a push
// will actually do about it.
func TestAutomationStatusReportsAnOrphanAsAnOrphan(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-2", Name: "invoices", CronExpr: "0 5 * * *", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"invoices.json": "job-2"},
		map[string]string{"invoices.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, nil)

	out, _, err := runAutomationCLI(t, dir, "automation", "status", "--json")
	if err == nil {
		t.Fatal("an orphan must not exit zero")
	}
	if !strings.Contains(err.Error(), "--prune") {
		t.Errorf("the exit message does not say what a push will do: %v", err)
	}
	if strings.Contains(err.Error(), "could not be checked") {
		t.Errorf("the orphan was scored as unreadable: %v", err)
	}
	report := &automationStatusReport{}
	decodeJSONInto(t, out, report)
	if len(report.Remote.Automations) != 1 || !report.Remote.Automations[0].Orphan {
		t.Fatalf("the row is not marked as an orphan: %+v", report.Remote.Automations)
	}
	if report.Remote.Automations[0].Problem != "" {
		t.Errorf("the orphan carries a problem: %q", report.Remote.Automations[0].Problem)
	}
	// And the tree verdict is drifted with the orphan sentence, not unknown.
	if v := verdictOfAutomationStatus(report, 0); v.Verdict != verdictDrifted {
		t.Errorf("verdict = %q (%s), want drifted", v.Verdict, v.Detail)
	}
}

// --- the review's four convergence / production refusals ---------------------

// TestAutomationZeroRateLimitIsRefusedAtParse: 0 is the one value on this
// surface that can never converge, and it fails DIFFERENTLY on each write path,
// so neither server answer would tell the reader what is wrong with the file.
//
// On an UPDATE the server reads an explicit 0 as "reset to the organization
// default" and stores NULL, so the row reads back with no limit: the delta then
// compares 0 against nil for ever and every push re-sends the field, succeeds,
// and prints its own "the row still differs" note. On a CREATE the server
// refuses `v <= 0` outright.
func TestAutomationZeroRateLimitIsRefusedAtParse(t *testing.T) {
	for _, field := range []string{"rateLimitPerMinute", "rateLimitPerDay"} {
		_, err := parseAutomationFile("a.json", `{"cronExpr": "0 2 * * *", "`+field+`": 0}`)
		if err == nil {
			t.Fatalf("%s: 0 must be refused before anything is sent", field)
		}
		if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "Leave") {
			t.Errorf("%s: the refusal does not name the field and the omit-the-key spelling: %v", field, err)
		}
		// A positive value is untouched, and so is an absent one.
		if _, ok := parseAutomationFile("a.json", `{"`+field+`": 5}`); ok != nil {
			t.Errorf("%s: a positive limit must parse: %v", field, ok)
		}
	}
	if _, err := parseAutomationFile("a.json", cronFile); err != nil {
		t.Errorf("a file declaring no rate limit must parse: %v", err)
	}
}

// TestAutomationPushRefusesABoundFolderWithNoFeature: a manifest without
// "featureID" is legal and the lock deliberately stores none, so a folder that
// creates nothing reaches the listing with an empty one — and an empty filter
// drops every row rather than failing.
//
// Read as "every automation is gone", that is three wrong answers at once: each
// bound file is refused as a broken binding naming an empty feature, `status`
// disagrees with `push` about the same folder, and with --prune the drift guard
// finds no row for any orphan and deletes each one unguarded. So push demands
// the feature before it lists, with the same advice `status` gives.
func TestAutomationPushRefusesABoundFolderWithNoFeature(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 6 * * *", Enabled: true})
	dir := t.TempDir()
	manifest, lock := automationStack(f, "",
		map[string]string{"nightly.json": "job-1", "gone.json": "job-2"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	// --prune, so the unguarded-delete leg is the one under test: `gone.json` is
	// bound and missing from the folder, which is exactly an orphan.
	_, stderr, err := runAutomationCLI(t, dir, "automation", "push", "--prune")
	if err == nil {
		t.Fatal("a bound folder naming no feature must be refused, not listed with an empty filter")
	}
	if !strings.Contains(err.Error(), "featureID") {
		t.Errorf("the refusal does not name the missing key: %v\n%s", err, stderr)
	}
	if len(f.deleted) != 0 {
		t.Errorf("--prune deleted %v with no listing to guard it", f.deleted)
	}
	if len(f.updates) != 0 || len(f.created) != 0 {
		t.Errorf("the refused push still wrote: %d update(s), %d create(s)", len(f.updates), len(f.created))
	}
}

// TestAutomationPushStopsOnAnInstanceFailure: failure is data on this surface,
// and that is right for a per-file refusal and wrong for a 5xx or a 429.
//
// Forty files against an instance mid-deploy would otherwise produce forty
// failed writes at full speed and a half-applied folder. Every write so far is
// recorded in the lock, so stopping is safe to retry — and the files that were
// never attempted are reported as unattempted rather than as failures.
func TestAutomationPushStopsOnAnInstanceFailure(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-a", Name: "a", CronExpr: "0 6 * * *", Enabled: true})
	f.AddAutomation(&api.Automation{ID: "job-b", Name: "b", CronExpr: "0 6 * * *", Enabled: true})
	f.failUpdate["job-a"] = 503
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"a.json": "job-a", "b.json": "job-b"},
		map[string]string{"a.json": anchoredAt(baseTime), "b.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{
		"a.json": cronFile, "b.json": cronFile,
	})

	_, stderr, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a 503 must not report success")
	}
	if !strings.Contains(err.Error(), "not attempted") {
		t.Errorf("the report does not say the rest were never tried: %v\n%s", err, stderr)
	}
	for _, u := range f.updates {
		if u.ID == "job-b" {
			t.Error("b.json was pushed into an instance that had just answered 503")
		}
	}
}

// TestAutomationTruncatedListingNamesAnOlderBackend: the truncation refusal is
// scored off `total`, which an instance predating the `featureID` parameter
// reports for the whole ORGANIZATION.
//
// Told to "split the feature", the reader of a three-automation feature has
// nothing to split and no --force past it. The listing is still refused — an
// unfiltered page that did not fit is a prefix of the organization, so rows of
// this feature may be missing from it — but the message names the real fix.
func TestAutomationTruncatedListingNamesAnOlderBackend(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.ignoreFeatureFilter = true
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	f.AddAutomation(&api.Automation{ID: "job-9", Name: "elsewhere", FeatureID: "collection-other", Enabled: true})
	f.totalOverride = 500
	dir := t.TempDir()
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, dir, manifest, lock, map[string]string{"nightly.json": cronFile})

	_, _, err := runAutomationCLI(t, dir, "automation", "push")
	if err == nil {
		t.Fatal("a truncated listing must be refused whoever truncated it")
	}
	if !strings.Contains(err.Error(), "older than this CLI") {
		t.Errorf("the refusal blames the feature rather than the instance: %v", err)
	}
	if strings.Contains(err.Error(), "Split the feature") {
		t.Errorf("the reader is told to split a feature that may hold three automations: %v", err)
	}
}
