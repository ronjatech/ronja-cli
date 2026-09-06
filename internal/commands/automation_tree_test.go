package commands

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The automation folder as the TREE commands see it.
//
// `sync status` and `sync check` walk every kind, and a kind they do not handle
// does not fail loudly: it falls through to another kind's computation and
// answers confidently about the wrong thing. Phase A already fixed folderKindAt
// — an automation folder used to come back `known=false` and score the whole
// tree exit 2 — and these are the two legs above it.

// automationTree lays out one automation folder inside a directory a tree
// command can be pointed at, and returns both paths.
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

// TestSyncStatusSeesAnAutomationFolder: the tree verdict for this kind is
// computed by the folder's OWN status function, not by a fall-through to the
// workflow one — which is what `default:` would have given it.
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

// TestSyncStatusNeverDeployedAutomationFolderIsDrifted is the false green this
// rule exists for: a repository holding an automation nobody has ever pushed is
// not a healthy repository, and a gate that passed it would pass the one run
// that matters most.
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

// TestSyncCheckResolvesAutomationFieldReferences: an automation's references are
// a structured FIELD, so markers.Scan finds nothing in one — which means the
// pipeline fall-through reported every automation folder as making no references
// at all, `clean`, with a dangling note reference sitting in it.
func TestSyncCheckResolvesAutomationFieldReferences(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	tree, _ := automationTree(t, f,
		map[string]string{"nightly.json": `{
  "cronExpr": "0 2 * * *",
  "references": [{"kind": "note", "resourceID": "policy"}]
}`}, nil, nil,
		map[string]wfdir.Dependency{"policy": {Kind: "note"}}, nil)

	// Declared and unbound: `unresolved`, decided with no server at all.
	out, err := runCLI(t, tree, "sync", "check", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want broken\n%s", code, err, out)
	}
	report := decodeJSON(t, out)
	folders := report["folders"].([]any)
	folder := folders[0].(map[string]any)
	edges, _ := folder["edges"].([]any)
	// The FIELD edge, not the manifest-level declaration one: both are emitted
	// (an unbound alias is a fact about the manifest AND about every field that
	// writes it), and only the field edge proves this kind's references were read
	// at all.
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

// TestFolderStemsClaimsAutomationNames: an automation's name is its filename, so
// a dependency declared under the same word is dead text that reads as if it
// were in force — the collision `folderStems` exists to warn about, and which
// this kind returned nothing for until the push existed.
func TestFolderStemsClaimsAutomationNames(t *testing.T) {
	stems := folderStems(wfdir.AutomationKind, map[string]string{"nightly.json": "{}", "a/b.json": "{}"})
	if len(stems) != 2 {
		t.Fatalf("stems = %v", stems)
	}
	joined := strings.Join(stems, ",")
	if !strings.Contains(joined, "nightly") || !strings.Contains(joined, "b") {
		t.Errorf("stems = %v", stems)
	}
	// Every other kind still claims nothing, which is what keeps this inert for
	// the folders that existed before it.
	if s := folderStems(wfdir.WorkflowKind, map[string]string{"main.py": ""}); s != nil {
		t.Errorf("a workflow folder claimed stems: %v", s)
	}
}

// TestAutomationFieldRefsCountAsAUse: markers.Scan reads a .json file and finds
// nothing, so without the field leg every dependency an automation folder
// declares would be reported as dead config on every push and status — the
// warning crying wolf at the one kind with no other way to say what it uses.
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
	// Without the field leg it IS reported — which is what the leg fixes, and
	// pinning it here is what stops somebody dropping the argument as unused.
	withoutFields := checkAliases(manifest, sel, codec, files, nil, nil)
	if len(withoutFields.Warnings) == 0 {
		t.Errorf("the source-only pre-flight found the use after all; this test no longer proves anything")
	}
}

// TestLiteralBindTargetInAnAutomationField: an id a bind already points at,
// written literally in a field, is silently NON-PORTABLE — nothing drifts,
// because an id compares equal to itself, and there is no symptom at all until
// the folder is pushed to the second stack.
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

// TestPrintLocalWorkOffersTheAutomationVerbs: `ronja context` is the handoff
// command, so it is where a lost reader decides what to do next. Before this
// kind had a verb list it fell through to the "a kind with no verbs yet" branch
// and said nothing at all — better than offering `ronja wf`, and still a
// capability nobody could find.
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
	// The two things worth knowing before the first edit, and the reason a verb
	// list alone is not enough: there is nothing to publish, and an omitted field
	// is an unmanaged one.
	if !strings.Contains(printed, "no publish") {
		t.Errorf("the note does not say there is no publish step:\n%s", printed)
	}
}
