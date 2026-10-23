package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The alias kinds an automation's reference set needed and did not have —
// dataapp, mcpserver and feature, plus "@feature" for the automation's own
// feature — and the one SQL a workflow carries outside its files: a select
// parameter's optionsQuery.

// TestAutomationReferencesResolveTheNewAliasKinds: a `dataapp`, `mcpserver` and
// `feature` reference written as an alias resolves through the stack's bind,
// exactly as a `note` one always has. Before these kinds existed the resolver
// refused every such reference outright.
func TestAutomationReferencesResolveTheNewAliasKinds(t *testing.T) {
	deps := map[string]wfdir.Dependency{
		"dashboard": {Kind: markers.KindDataApp},
		"crm":       {Kind: markers.KindMCPServer},
		"finance":   {Kind: markers.KindFeature},
	}
	codec := newAliasCodec(deps, map[string]string{
		"dashboard": "data_app-1", "crm": "mcpsrv-1", "finance": "collection-fin",
	})
	file, err := parseAutomationFile("nightly.json", `{
		"action": {"kind": "agent"},
		"references": [
			{"kind": "dataapp", "resourceID": "dashboard"},
			{"kind": "mcpserver", "resourceID": "crm"},
			{"kind": "feature", "resourceID": "finance"}
		]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := resolveAutomationRefs(file, codec, "main", "collection-own"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := []string{}
	for _, r := range *file.References {
		got = append(got, r.ResourceID)
	}
	if want := "data_app-1,mcpsrv-1,collection-fin"; strings.Join(got, ",") != want {
		t.Fatalf("resolved = %v, want %s", got, want)
	}
}

// TestAutomationFeatureSelfRefResolvesToTheStacksFeature: "@feature" names the
// feature the selected stack deploys to, so the same file sends a different id
// to each stack — and a stack with no feature is refused rather than sent the
// token as an id.
func TestAutomationFeatureSelfRefResolvesToTheStacksFeature(t *testing.T) {
	parse := func() *automationFile {
		file, err := parseAutomationFile("nightly.json", `{"action": {"kind": "agent"},
			"references": [{"kind": "feature", "resourceID": "@feature"}]}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return file
	}
	file := parse()
	if err := resolveAutomationRefs(file, aliasCodec{}, "main", "collection-main"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := (*file.References)[0].ResourceID; got != "collection-main" {
		t.Fatalf("@feature resolved to %q, want the stack's feature", got)
	}
	file = parse()
	err := resolveAutomationRefs(file, aliasCodec{}, "dev", "")
	if err == nil || !strings.Contains(err.Error(), "@feature") {
		t.Fatalf("a stack with no feature resolved @feature: %v", err)
	}
}

// TestSearchCoversEveryAutomationReferenceKind: `ronja bind` answers each new
// kind through the search endpoint, which reports dataapp, mcpserver and feature
// hits. A kind missing from searchKindOf reads "unknown_kind", which would tell
// the reader to upgrade a CLI that already resolves it.
func TestSearchCoversEveryAutomationReferenceKind(t *testing.T) {
	for _, kind := range automationReferenceKinds {
		if _, ok := searchKindOf(kind); !ok {
			t.Errorf("searchKindOf(%q) has no search kind", kind)
		}
	}
}

// TestPushOfAnAliasOptionsQueryIsUpToDateOnTheSecondRun is the acceptance test
// for optionsQuery under the codec: an alias there goes out as the id, the row's
// id comes back as the alias, and a second push changes nothing — no parameter
// patch, no drift.
func TestPushOfAnAliasOptionsQueryIsUpToDateOnTheSecondRun(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "read(\"{{ ref('orders') }}\")\n")
	setManifestParameters(t, root, api.WorkflowParameter{
		Name: "region", Type: "select", OptionsQuery: "SELECT r FROM {{ ref('orders') }}",
	})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %d, want 1", len(f.created))
	}
	if got := f.created[0].Parameters; len(got) != 1 || got[0].OptionsQuery != "SELECT r FROM {{ ref('table-orders') }}" {
		t.Fatalf("the server was sent parameters %+v, want the alias resolved", got)
	}
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := m.DeclaredParameters()[0].OptionsQuery; got != "SELECT r FROM {{ ref('orders') }}" {
		t.Fatalf("the manifest was rewritten to %q", got)
	}

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("second push was not up to date — the optionsQuery is compared in the wrong form:\n%s", out)
	}
	if len(f.parameterPatches) != 0 {
		t.Fatalf("the second push patched parameters: %+v", f.parameterPatches)
	}
}

// TestCodecRoundTripsAnOptionsQuery pins both directions of the parameter
// rewrite and that neither touches the caller's slice.
func TestCodecRoundTripsAnOptionsQuery(t *testing.T) {
	codec := newAliasCodec(tableDep(), map[string]string{"orders": "table-orders"})
	disk := []api.WorkflowParameter{{Name: "r", Type: "select", OptionsQuery: "SELECT 1 FROM {{ ref('orders') }}"}}
	wire := codec.parametersToWire(disk)
	if wire[0].OptionsQuery != "SELECT 1 FROM {{ ref('table-orders') }}" {
		t.Fatalf("toWire = %q", wire[0].OptionsQuery)
	}
	if disk[0].OptionsQuery != "SELECT 1 FROM {{ ref('orders') }}" {
		t.Fatal("toWire rewrote the caller's slice")
	}
	if back := codec.parametersToDisk(wire); back[0].OptionsQuery != disk[0].OptionsQuery {
		t.Fatalf("toDisk = %q", back[0].OptionsQuery)
	}
	if codec.parametersToWire(nil) != nil {
		t.Fatal("nil parameters became non-nil — an unmanaged folder would start patching")
	}
}

// TestAnAliasUsedOnlyInAnOptionsQueryIsNotDeadConfig: the pre-flight reads the
// optionsQuery beside the files, so a dependency used only there is not
// reported unused, and a bare name there is refused like one in a file.
func TestAnAliasUsedOnlyInAnOptionsQueryIsNotDeadConfig(t *testing.T) {
	m := &wfdir.Manifest{FormatVersion: 3, Kind: wfdir.KindWorkflow, Dependencies: tableDep()}
	m.SetParameters([]api.WorkflowParameter{{Name: "r", Type: "select", OptionsQuery: "SELECT 1 FROM {{ ref('orders') }}"}})
	sel := wfdir.Selection{Name: "dev", Bind: map[string]string{"orders": "table-orders"}}
	report := checkAliases(m, sel, newAliasCodec(m.Dependencies, sel.Bind), map[string]string{"main.py": "x\n"}, nil, nil)
	if len(report.Warnings) != 0 || len(report.Refusals) != 0 {
		t.Fatalf("report = %+v, want clean", report)
	}

	m.SetParameters([]api.WorkflowParameter{{Name: "r", Type: "select", OptionsQuery: "SELECT 1 FROM {{ ref('customers') }}"}})
	report = checkAliases(m, sel, newAliasCodec(m.Dependencies, sel.Bind), map[string]string{"main.py": "{{ ref('orders') }}\n"}, nil, nil)
	if err := report.err(); err == nil || !strings.Contains(err.Error(), "parameters[0].optionsQuery") {
		t.Fatalf("a bare name in an optionsQuery was not refused: %v", err)
	} else if !strings.Contains(err.Error(), "refuse the save as an unresolved table") || strings.Contains(err.Error(), "store the ID") {
		// The server binds an optionsQuery's tables by id and refuses a name
		// there at save, rather than storing the id as it does for a file.
		t.Fatalf("the refusal misstates what the server does with an optionsQuery name: %v", err)
	}
}

// TestWriteTargetAndOutputRoundTripThroughWriteMarkers: a write-capable data
// source and an output resolve inside `{{ write(...) }}` — the target to its
// bound id, the output to the id its stack binds — and come back as the alias,
// so a second push is up to date. An unbound output goes out as its name, the
// form the runtime creates in the stack's own feature.
func TestWriteTargetAndOutputRoundTripThroughWriteMarkers(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "w(\"{{ write('events') }}\")\nw(\"{{ write('orders_daily') }}\")\n")
	m := loadAnyManifest(t, root)
	m.Dependencies = map[string]wfdir.Dependency{
		"events":       {Kind: markers.KindTable, Write: true},
		"orders_daily": {Kind: markers.KindTable, Output: true},
	}
	st := m.Stacks["dev"]
	st.Bind = map[string]string{"events": "table-events", "orders_daily": "table-daily"}
	m.Stacks["dev"] = st
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	want := "w(\"{{ write('table-events') }}\")\nw(\"{{ write('table-daily') }}\")\n"
	if got := f.FileContents(workflowIDOf(t, root))["main.py"]; got != want {
		t.Fatalf("the server was sent %q, want %q", got, want)
	}
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("second push: %v\n%s", err, out)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("second push was not up to date:\n%s", out)
	}

	codec := newAliasCodec(map[string]wfdir.Dependency{"orders_daily": {Kind: markers.KindTable, Output: true}}, nil)
	if got := codec.toWire("{{ write('orders_daily') }}"); got != "{{ write('orders_daily') }}" {
		t.Fatalf("an unbound output was rewritten to %q", got)
	}
	m.Dependencies = map[string]wfdir.Dependency{"orders_daily": {Kind: markers.KindTable, Output: true}}
	m.FormatVersion = 7
	report := checkAliases(m, wfdir.Selection{Name: "main"}, codec, map[string]string{"main.py": "{{ write('orders_daily') }}"}, nil, nil)
	if err := report.err(); err != nil {
		t.Fatalf("an unbound output was refused: %v", err)
	}
}

// TestPushOfADeclaredSecretAliasIsNotRefused: the unresolvable-secret leg of
// misusedAliases refused a secret alias the folder DECLARES and binds, so a
// workflow could never name a secret by alias at all.
func TestPushOfADeclaredSecretAliasIsNotRefused(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := aliasWorkflowFolder(t, f, "t = \"{{ secret('crm', 'token') }}\"\nread(\"{{ ref('orders') }}\")\n")
	m := loadAnyManifest(t, root)
	m.Dependencies["crm"] = wfdir.Dependency{Kind: markers.KindSecret}
	st := m.Stacks["dev"]
	st.Bind["crm"] = "secret-crm"
	m.Stacks["dev"] = st
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push of a declared secret alias: %v\n%s", err, out)
	}
	if got := f.FileContents(workflowIDOf(t, root))["main.py"]; !strings.Contains(got, "{{ secret('secret-crm', 'token') }}") {
		t.Fatalf("the server was sent %q", got)
	}
}

// TestWriteMarkerNamingAReadOnlyDependencyIsRefused: `{{ write('events') }}`
// with `events` declared a plain (read) table would go out as a NEW table's
// name — the bound row never written, the push green. Refused, naming the two
// keys that would make it mean something.
func TestWriteMarkerNamingAReadOnlyDependencyIsRefused(t *testing.T) {
	m := &wfdir.Manifest{FormatVersion: 7, Kind: wfdir.KindWorkflow,
		Dependencies: map[string]wfdir.Dependency{"events": {Kind: markers.KindTable}}}
	sel := wfdir.Selection{Name: "main", Bind: map[string]string{"events": "table-e"}}
	report := checkAliases(m, sel, newAliasCodec(m.Dependencies, sel.Bind),
		map[string]string{"main.py": "{{ write('events') }}\n"}, nil, nil)
	if err := report.err(); err == nil || !strings.Contains(err.Error(), `"write": true`) {
		t.Fatalf("a write marker naming a read dependency was not refused: %v", err)
	}
	m.Dependencies["events"] = wfdir.Dependency{Kind: markers.KindTable, Write: true}
	report = checkAliases(m, sel, newAliasCodec(m.Dependencies, sel.Bind),
		map[string]string{"main.py": "{{ write('events') }}\n"}, nil, nil)
	if err := report.err(); err != nil {
		t.Fatalf("a declared write target was refused: %v", err)
	}
}
