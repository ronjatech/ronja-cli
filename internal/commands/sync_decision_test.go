package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// decisionFolder carries no client: a local decision must not touch the network.
func decisionFolder(t *testing.T, kind wfdir.Kind, stack, featureID string) *folder {
	t.Helper()
	m := &wfdir.Manifest{Kind: kind.Name, Title: "T", Entrypoint: "main.py"}
	if stack != "" {
		m.Stacks = map[string]wfdir.Stack{stack: {
			URL: "http://x.test", TenantID: testTenantID, FeatureID: featureID,
		}}
	}
	lock := &wfdir.Lock{}
	return &folder{
		Root: t.TempDir(), Kind: kind, Manifest: m, Lock: lock, Stack: stack,
		Key:      wfdir.InstanceKey{URL: "http://x.test", TenantID: testTenantID},
		Binding:  wfdir.Binding{FeatureID: featureID},
		Bound:    stack != "",
		Resolved: &config.Resolved{URL: "http://x.test", TenantID: testTenantID},
	}
}

// Per file, or six bound files and one new one read as a bound folder and the
// seventh table slips past a create gate.
func TestPipelineApplyDecisionIsPerFile(t *testing.T) {
	f := decisionFolder(t, wfdir.PipelineKind, "prod", "feat-1")
	f.Binding.Tables = map[string]string{"orders.sql": "tbl-1"}

	for _, tc := range []struct {
		path   string
		action applyAction
		id     string
	}{
		{"orders.sql", applyActionUpdate, "tbl-1"},
		{"customers.sql", applyActionCreate, ""},
	} {
		d := pipelineApplyDecision(f, tc.path)
		if d.Action != tc.action || d.ID != tc.id {
			t.Errorf("%s: action = %q id = %q, want %q / %q", tc.path, d.Action, d.ID, tc.action, tc.id)
		}
		if d.Path != tc.path {
			t.Errorf("%s: the decision names path %q", tc.path, d.Path)
		}
	}
}

func TestAutomationApplyDecisionIsPerFileAndNamesADisarmedAnchor(t *testing.T) {
	f := decisionFolder(t, wfdir.AutomationKind, "prod", "feat-1")
	f.Binding.Automations = map[string]string{
		"nightly.json": "job-1",
		"weekly.json":  "job-2",
	}
	// weekly is bound with no anchor: it disarms both guards while looking like
	// an ordinary update.
	f.Lock.SetAutomationSeen("prod", "nightly.json", "job-1", "2026-09-01T00:00:00Z")
	f.Lock.SetAutomationSeen("prod", "weekly.json", "job-2", "")

	for _, tc := range []struct {
		path   string
		action applyAction
		reason string
	}{
		{"nightly.json", applyActionUpdate, ""},
		{"weekly.json", applyActionUnanchored, syncReasonNoDriftAnchor},
		{"hourly.json", applyActionCreate, ""},
	} {
		d := automationApplyDecision(f, tc.path)
		if d.Action != tc.action || d.Reason != tc.reason {
			t.Errorf("%s: action = %q reason = %q, want %q / %q", tc.path, d.Action, d.Reason, tc.action, tc.reason)
		}
		if d.Action == applyActionUnanchored && !strings.Contains(d.Detail, "job-2") {
			t.Errorf("%s: an unanchored decision must name the row it cannot speak for: %q", tc.path, d.Detail)
		}
	}
}

// Not a refusal either, so the push loops keep writing such a row.
func TestUnanchoredIsNeitherCreateNorRefusal(t *testing.T) {
	f := decisionFolder(t, wfdir.AutomationKind, "prod", "feat-1")
	f.Binding.Automations = map[string]string{"weekly.json": "job-2"}
	d := automationApplyDecision(f, "weekly.json")
	if d.Creates() {
		t.Error("a bound row with no anchor was counted as a create")
	}
	if d.Refused() || d.Err() != nil {
		t.Error("a disarmed anchor refused here, which would change what the push loop does")
	}
	if err := errFromDecisions([]applyDecision{d}); err != nil {
		t.Errorf("errFromDecisions = %v, want nil for an unanchored unit", err)
	}
}

func TestFolderGrainKindsHaveOneCreationUnit(t *testing.T) {
	wf := decisionFolder(t, wfdir.WorkflowKind, "prod", "feat-1")
	if d := workflowApplyDecision(wf); !d.Creates() || d.Path != "" {
		t.Errorf("an unbound workflow folder = %+v, want a create with no path", d)
	}
	wf.Binding.WorkflowID = "wf-1"
	if d := workflowApplyDecision(wf); d.Action != applyActionUpdate || d.ID != "wf-1" {
		t.Errorf("a bound workflow folder = %+v, want an update naming wf-1", d)
	}

	app := decisionFolder(t, wfdir.DataAppKind, "prod", "feat-1")
	if d := appApplyDecision(app); !d.Creates() {
		t.Errorf("an unbound data-app folder = %+v, want a create", d)
	}
	app.Binding.DataAppID = "app-1"
	if d := appApplyDecision(app); d.Action != applyActionUpdate || d.ID != "app-1" {
		t.Errorf("a bound data-app folder = %+v, want an update naming app-1", d)
	}

	paths := []string{"a.sql", "b.sql"}
	if got := len(applyDecisionsFor(app, paths)); got != 1 {
		t.Errorf("applyDecisionsFor(data app) gave %d decisions, want 1", got)
	}
	pl := decisionFolder(t, wfdir.PipelineKind, "prod", "feat-1")
	if got := len(applyDecisionsFor(pl, paths)); got != len(paths) {
		t.Errorf("applyDecisionsFor(pipeline) gave %d decisions, want %d", got, len(paths))
	}
}

func TestApplyDecisionRefusesACreateWithNoFeature(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   wfdir.Kind
		decide func(*folder) applyDecision
		want   string
	}{
		{"pipeline", wfdir.PipelineKind,
			func(f *folder) applyDecision { return pipelineApplyDecision(f, "orders.sql") },
			"has no table yet"},
		{"automation", wfdir.AutomationKind,
			func(f *folder) applyDecision { return automationApplyDecision(f, "nightly.json") },
			"has no automation yet"},
		{"workflow", wfdir.WorkflowKind, workflowApplyDecision, "no feature recorded"},
		{"data app", wfdir.DataAppKind, appApplyDecision, "no feature to create the data app in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.decide(decisionFolder(t, tc.kind, "prod", ""))
			if !d.Refused() {
				t.Fatalf("decision = %+v, want a refusal", d)
			}
			if d.Reason != syncReasonNoFeature {
				t.Errorf("reason = %q, want %q", d.Reason, syncReasonNoFeature)
			}
			if !strings.Contains(d.Detail, tc.want) {
				t.Errorf("detail = %q, want it to say %q", d.Detail, tc.want)
			}
			if d.Err() == nil || d.Err().Error() != d.Detail {
				t.Errorf("Err() = %v, want the same sentence as Detail", d.Err())
			}
			if tc.kind.Name == wfdir.KindWorkflow && !errors.Is(d.Err(), errNoFeature) {
				t.Error("the workflow refusal lost its errNoFeature wrapping, which `sync check` reads")
			}
			if d.Creates() {
				t.Error("a refused unit reported itself as a create")
			}
		})
	}
}

// A bound unit's feature comes from the row, so refusing it would break every
// folder whose manifest never recorded one.
func TestApplyDecisionDoesNotDemandAFeatureForAnUpdate(t *testing.T) {
	f := decisionFolder(t, wfdir.WorkflowKind, "prod", "")
	f.Binding.WorkflowID = "wf-1"
	if d := workflowApplyDecision(f); d.Refused() {
		t.Errorf("a bound workflow with no featureID was refused: %s", d.Detail)
	}
	pl := decisionFolder(t, wfdir.PipelineKind, "prod", "")
	pl.Binding.Tables = map[string]string{"orders.sql": "tbl-1"}
	if d := pipelineApplyDecision(pl, "orders.sql"); d.Refused() {
		t.Errorf("a bound table with no featureID was refused: %s", d.Detail)
	}
}

// openFolder treats these stack states as a hard error or a silent manifest
// rewrite; a tree command needs them as never-green reasons.
func TestDecideApplyFolderStackStatesAndManifestRewrites(t *testing.T) {
	here := wfdir.InstanceKey{URL: "http://x.test", TenantID: testTenantID}
	stacks := func(entries map[string]wfdir.Stack) *wfdir.Manifest {
		return &wfdir.Manifest{Kind: wfdir.KindPipeline, Stacks: entries}
	}

	for _, tc := range []struct {
		name   string
		m      *wfdir.Manifest
		want   wfdir.InstanceKey
		stack  string
		reason string
	}{
		{
			name: "declared here",
			m:    stacks(map[string]wfdir.Stack{"prod": {URL: "http://x.test", TenantID: testTenantID}}),
			want: here, stack: "prod", reason: "",
		},
		{
			name: "another organization",
			m:    stacks(map[string]wfdir.Stack{"prod": {URL: "http://x.test", TenantID: "ten-other"}}),
			want: here, stack: "prod", reason: syncReasonStackElsewhere,
		},
		{
			name: "another instance",
			m:    stacks(map[string]wfdir.Stack{"prod": {URL: "http://y.test", TenantID: testTenantID}}),
			want: here, stack: "prod", reason: syncReasonStackElsewhere,
		},
		{
			name: "names stacks, not this one",
			m:    stacks(map[string]wfdir.Stack{"dev": {URL: "http://x.test", TenantID: testTenantID}}),
			want: here, stack: "prod", reason: syncReasonStackAbsent,
		},
		{
			name: "names no stacks at all",
			m:    stacks(nil),
			want: here, stack: "prod", reason: syncReasonUnnamedStack,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, detail := decideApplyFolder(tc.m, &wfdir.Lock{}, tc.want, tc.stack)
			if reason != tc.reason {
				t.Fatalf("reason = %q (%s), want %q", reason, detail, tc.reason)
			}
			if reason != "" && detail == "" {
				t.Error("a never-green reason with no sentence beside it")
			}
		})
	}

	// The two acts that rewrite a committed manifest. (a) A legacy entry for the
	// place the named stack points, which Manifest.Record drops on the next write.
	beside := stacks(map[string]wfdir.Stack{"prod": {URL: "http://x.test", TenantID: testTenantID}})
	beside.SetBinding(here, wfdir.Binding{WorkflowID: "wf-old", FeatureID: "feat-old"})
	reason, detail := decideApplyFolder(beside, &wfdir.Lock{}, here, "prod")
	if reason != syncReasonManifestRewrite {
		t.Fatalf("reason = %q (%s), want %q", reason, detail, syncReasonManifestRewrite)
	}
	if !strings.Contains(detail, "instances") {
		t.Errorf("detail = %q, want it to name the key it is about", detail)
	}

	// ⚠️ With no --stack it is not a rewrite: Manifest.Record's unnamed branch
	// sets the binding in place before the instances[] drop. Refusing it would
	// leave a legacy folder no route to green.
	legacy := stacks(nil)
	legacy.SetBinding(here, wfdir.Binding{WorkflowID: "wf-old", FeatureID: "feat-old"})
	if reason, detail := decideApplyFolder(legacy, &wfdir.Lock{}, here, ""); reason != "" {
		t.Errorf("a legacy folder with no --stack was refused %q (%s), and a push there migrates nothing", reason, detail)
	}
	elsewhere := stacks(map[string]wfdir.Stack{"prod": {URL: "http://x.test", TenantID: testTenantID}})
	elsewhere.SetBinding(wfdir.InstanceKey{URL: "http://y.test", TenantID: "ten-other"}, wfdir.Binding{WorkflowID: "wf-elsewhere"})
	if reason, detail := decideApplyFolder(elsewhere, &wfdir.Lock{}, here, "prod"); reason != "" {
		t.Errorf("an instances entry for another place was refused %q (%s)", reason, detail)
	}

	// (b) A stranded lock stack: resolveStackForFolder accepts it (selectNamed
	// adopts it) and adoptStack then writes it into ronja.json.
	stranded := stacks(map[string]wfdir.Stack{"dev": {URL: "http://x.test", TenantID: testTenantID}})
	lock := &wfdir.Lock{}
	lock.SetBinding("prod", wfdir.Binding{WorkflowID: "wf-stranded"})
	if reason, detail := decideApplyFolder(stranded, lock, here, "prod"); reason != syncReasonManifestRewrite {
		t.Errorf("reason = %q (%s), want %q for a stranded lock stack", reason, detail, syncReasonManifestRewrite)
	}
	ordinary := stacks(map[string]wfdir.Stack{"prod": {URL: "http://x.test", TenantID: testTenantID}})
	if reason, detail := decideApplyFolder(ordinary, lock, here, "prod"); reason != "" {
		t.Errorf("an ordinary declared stack was refused: %s (%s)", reason, detail)
	}
}

// The reason lets a tree report group it; the sentence stays the alias
// pre-flight's.
func TestDecideApplyBindReportsAnUnresolvedAlias(t *testing.T) {
	f := decisionFolder(t, wfdir.PipelineKind, "prod", "feat-1")
	// Without the alias format version the pre-flight refuses for another reason.
	f.Manifest.FormatVersion = wfdir.ManifestFormatVersion
	f.Manifest.Dependencies = map[string]wfdir.Dependency{"orders": {Kind: "table"}}
	files := map[string]string{"summary.sql": "select * from {{ ref('orders') }}"}

	report, reason, detail := decideApplyBind(f, files)
	if reason != syncReasonUnresolvedBind {
		t.Fatalf("reason = %q (%s), want %q", reason, detail, syncReasonUnresolvedBind)
	}
	if len(report.Refusals) == 0 {
		t.Error("the report came back with no refusals, so the reason was invented rather than read")
	}
	if detail != report.err().Error() {
		t.Errorf("detail = %q, want the alias pre-flight's own sentence %q", detail, report.err().Error())
	}

	f.Bind = map[string]string{"orders": "table-1"}
	f.Codec = newAliasCodec(f.Manifest.Dependencies, f.Bind)
	if _, reason, detail := decideApplyBind(f, files); reason != "" {
		t.Errorf("a bound alias still refused: %s (%s)", reason, detail)
	}
}

// The given order is the order both file loops iterate.
func TestErrFromDecisionsTakesTheFirstRefusalInOrder(t *testing.T) {
	f := decisionFolder(t, wfdir.PipelineKind, "prod", "")
	decisions := applyDecisionsFor(f, []string{"a.sql", "b.sql"})
	err := errFromDecisions(decisions)
	if err == nil {
		t.Fatal("two unbound files with no feature were accepted")
	}
	if !strings.Contains(err.Error(), "a.sql") {
		t.Errorf("error = %v, want it to name the first file", err)
	}
	if errFromDecisions(nil) != nil {
		t.Error("an empty list produced an error")
	}
}
