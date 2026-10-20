package commands

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The automation folder as the tree commands see it. A kind `sync status` and
// `sync check` do not handle falls through to another kind's computation and
// answers confidently about the wrong thing, rather than failing.

func automationTree(t *testing.T, f *fakeAutomationInstance, files map[string]string,
	bound, seen map[string]string, deps map[string]wfdir.Dependency, bind map[string]string) (tree, root string) {
	t.Helper()
	tree = t.TempDir()
	root = filepath.Join(tree, "automations")
	manifest, lock := automationStack(f, "collection-1", bound, seen)
	manifest.Dependencies = deps
	if bind != nil {
		stack := manifest.Stacks["prod"]
		stack.Bind = bind
		manifest.Stacks["prod"] = stack
	}
	writeAutomationFolder(t, root, manifest, lock, files)
	return tree, root
}

// The verdict must come from the automation status function, not a `default:`
// fall-through to the workflow one.
func TestSyncStatusSeesAnAutomationFolder(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	tree, _ := automationTree(t, f,
		map[string]string{"nightly.json": `{"cronExpr": "0 2 * * *"}`},
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)}, nil, nil)

	out, err := runCLI(t, tree, "sync", "status", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want clean\n%s", code, err, out)
	}
	report := decodeJSON(t, out)
	folders := report["folders"].([]any)
	if len(folders) != 1 {
		t.Fatalf("folders = %v", folders)
	}
	folder := folders[0].(map[string]any)
	if folder["kind"] != wfdir.KindAutomation {
		t.Fatalf("kind = %v", folder["kind"])
	}
	if folder["verdict"] != verdictClean {
		t.Errorf("verdict = %v (%v)", folder["verdict"], folder["detail"])
	}
}

// An automation nobody has pushed must not pass a gate as clean.
func TestSyncStatusNeverDeployedAutomationFolderIsDrifted(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	tree, _ := automationTree(t, f,
		map[string]string{"nightly.json": `{"cronExpr": "0 2 * * *"}`}, nil, nil, nil, nil)

	out, err := runCLI(t, tree, "sync", "status", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want drifted\n%s", code, err, out)
	}
	report := decodeJSON(t, out)
	if report["verdict"] != verdictDrifted {
		t.Errorf("verdict = %v\n%s", report["verdict"], out)
	}
}

// An automation's references are a structured field markers.Scan cannot see, so
// a fall-through would report a dangling reference as clean.
func TestSyncCheckResolvesAutomationFieldReferences(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	tree, _ := automationTree(t, f,
		map[string]string{"nightly.json": `{
  "cronExpr": "0 2 * * *",
  "references": [{"kind": "note", "resourceID": "policy"}]
}`}, nil, nil,
		map[string]wfdir.Dependency{"policy": {Kind: "note"}}, nil)

	// Declared and unbound: `unresolved`, decided with no server.
	out, err := runCLI(t, tree, "sync", "check", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want broken\n%s", code, err, out)
	}
	report := decodeJSON(t, out)
	folders := report["folders"].([]any)
	folder := folders[0].(map[string]any)
	edges, _ := folder["edges"].([]any)
	// The field edge, not the manifest-level one that is also emitted: only it
	// proves the file's references were read.
	found := false
	for _, raw := range edges {
		edge := raw.(map[string]any)
		where := fmt.Sprint(edge["where"])
		if edge["ref"] == "policy" && strings.Contains(where, "nightly.json") {
			found = true
			if edge["verdict"] != edgeUnresolved {
				t.Errorf("verdict = %v, want unresolved: %v", edge["verdict"], edge)
			}
			if !strings.Contains(where, "references[0].resourceID") {
				t.Errorf("the edge does not name the field: %v", edge)
			}
		}
	}
	if !found {
		t.Errorf("no edge for the note reference inside the file:\n%s", out)
	}
}

// An automation's name is its filename, so a dependency declared under the same
// word is the collision folderStems warns about.
func TestFolderStemsClaimsAutomationNames(t *testing.T) {
	stems := folderStems(wfdir.AutomationKind, map[string]string{"nightly.json": "{}", "a/b.json": "{}"})
	if len(stems) != 2 {
		t.Fatalf("stems = %v", stems)
	}
	joined := strings.Join(stems, ",")
	if !strings.Contains(joined, "nightly") || !strings.Contains(joined, "b") {
		t.Errorf("stems = %v", stems)
	}
	if s := folderStems(wfdir.WorkflowKind, map[string]string{"main.py": ""}); s != nil {
		t.Errorf("a workflow folder claimed stems: %v", s)
	}
}

// markers.Scan finds nothing in a .json file, so without the field leg every
// declared dependency would be reported as dead config.
func TestAutomationFieldRefsCountAsAUse(t *testing.T) {
	files := map[string]string{"nightly.json": `{
  "cronExpr": "0 2 * * *",
  "action": {"kind": "workflow", "config": {"workflowID": "nightly"}}
}`}
	manifest := &wfdir.Manifest{
		Kind:         wfdir.KindAutomation,
		Dependencies: map[string]wfdir.Dependency{"nightly": {Kind: "workflow"}},
	}
	sel := wfdir.Selection{Name: "prod", Bound: true, Bind: map[string]string{"nightly": "workflow-1"}}
	codec := newAliasCodec(manifest.Dependencies, sel.Bind)

	withFields := checkAliases(manifest, sel, codec, files,
		folderStems(wfdir.AutomationKind, files), folderFieldRefs(wfdir.AutomationKind, "", files))
	for _, warning := range withFields.Warnings {
		if strings.Contains(warning, "no file in this folder uses it") {
			t.Errorf("a used dependency was reported as dead config: %q", warning)
		}
	}
	// Negative control: without the field leg it is reported.
	withoutFields := checkAliases(manifest, sel, codec, files, nil, nil)
	if len(withoutFields.Warnings) == 0 {
		t.Errorf("the source-only pre-flight found the use after all; this test no longer proves anything")
	}
}

// A bind target's id written literally is silently non-portable: nothing
// drifts until the folder is pushed to a second stack.
func TestLiteralBindTargetInAnAutomationField(t *testing.T) {
	files := map[string]string{"nightly.json": `{
  "action": {"kind": "workflow", "config": {"workflowID": "workflow-1"}}
}`}
	manifest := &wfdir.Manifest{
		Kind:         wfdir.KindAutomation,
		Dependencies: map[string]wfdir.Dependency{"nightly": {Kind: "workflow"}},
	}
	sel := wfdir.Selection{Name: "prod", Bound: true, Bind: map[string]string{"nightly": "workflow-1"}}
	codec := newAliasCodec(manifest.Dependencies, sel.Bind)

	report := checkAliases(manifest, sel, codec, files,
		folderStems(wfdir.AutomationKind, files), folderFieldRefs(wfdir.AutomationKind, "", files))
	if len(report.Refusals) == 0 {
		t.Fatalf("writing a bind target's id literally was accepted: %+v", report)
	}
	if !strings.Contains(strings.Join(report.Refusals, " "), "nightly") {
		t.Errorf("the refusal does not name the alias to use: %v", report.Refusals)
	}
}

func TestPrintLocalWorkOffersTheAutomationVerbs(t *testing.T) {
	stdout, restore := captureStdout(t)
	printLocalWork(&localWorkflow{Root: "/tmp/x", Title: "Orders", Kind: wfdir.KindAutomation})
	printed := restore(stdout)

	for _, want := range []string{"ronja automation status", "ronja automation push"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the note does not offer %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "ronja wf ") || strings.Contains(printed, "ronja pipeline ") {
		t.Errorf("the note offers another loop's verbs:\n%s", printed)
	}
	if !strings.Contains(printed, "no publish") {
		t.Errorf("the note does not say there is no publish step:\n%s", printed)
	}
}
