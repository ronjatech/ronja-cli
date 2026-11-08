package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The write gate's Space rule, on the imported tree: Sales (the tree's `main`
// stack) and Finance are in Space Dev, and collection-main in Space Main, the
// organization's default. Each case runs on a folder the feature rule does not
// cover — one with no folder id, and one whose stack is unmanaged — because a
// stack with a Space is held to it either way.

const mainSpaceFeature = "collection-main"

func spacedTree(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f, dir := gatedTree(t)
	f.pipe.AddFeature(mainSpaceFeature, "Sales (Main)", "organization")
	f.pipe.AddTable(&api.Table{ID: "table-main", Name: "main_orders", FeatureID: mainSpaceFeature, Kind: "dynamic"})
	f.codexes["cdx-main"] = api.Codex{ID: "cdx-main", FeatureID: mainSpaceFeature, Name: "Main contracts"}
	f.wf.AddWorkflow(&api.Workflow{ID: "workflow-main", Lifecycle: api.LifecycleLive, FeatureID: mainSpaceFeature,
		FeatureScope: "organization", Title: "Main close", RuntimeVersion: wfdir.RuntimeQuery},
		api.WorkflowFile{Path: "main.py", Content: "print('main')\n"})
	f.featureSpace = map[string]string{importFeature: "space-dev", financeFeature: "space-dev", mainSpaceFeature: "space-main"}
	f.wf.spaceNames = map[string]string{"space-dev": "Dev", "space-main": "Main"}
	f.wf.defaultSpace = "space-main"
	if f.wf.secretIDs == nil {
		f.wf.secretIDs = map[string]bool{}
	}
	f.wf.secretIDs["secret-crm"], f.wf.secretIDs["secret-main"] = true, true
	f.wf.secretFeatures = map[string]string{"secret-crm": financeFeature, "secret-main": mainSpaceFeature}
	return f, dir
}

// The two folder shapes the feature rule does not cover.
var spaceGateModes = []string{"no folder id", "unmanaged"}

// asSpaceStack declares Space Dev on the folder's main stack, and takes the
// folder out of the feature rule the way mode says. edit, when set, changes
// the manifest further.
func asSpaceStack(t *testing.T, root, mode string, edit func(m *wfdir.Manifest)) {
	t.Helper()
	m := loadAnyManifest(t, root)
	st := m.Stacks["main"]
	st.SpaceID, st.Space = "space-dev", "Dev"
	switch mode {
	case "no folder id":
		m.ID = ""
	case "unmanaged":
		st.Unmanaged = true
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	if st.Bind == nil {
		st.Bind = map[string]string{}
	}
	m.Stacks["main"] = st
	if edit != nil {
		edit(m)
	}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

// assertSpaceRefused fails unless err is bind_space_mismatch naming every want,
// with nothing but reads and validate sent.
func assertSpaceRefused(t *testing.T, f *fakeImportInstance, out string, err error, wants ...string) {
	t.Helper()
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != syncReasonBindSpaceMismatch {
		t.Fatalf("got %v, want bind_space_mismatch\n%s", err, out)
	}
	for _, want := range append([]string{`stack "main" is declared for Space Dev`}, wants...) {
		if !strings.Contains(r.Detail, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, r.Detail)
		}
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("a refused deploy sent %v", writes)
	}
}

func bindMain(alias, id string) func(m *wfdir.Manifest) {
	return func(m *wfdir.Manifest) { m.Stacks["main"].Bind[alias] = id }
}

func TestSpaceRuleRefusesAnOutputBoundIntoAnotherSpace(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode, func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "workflows/sales-report")
			asSpaceStack(t, root, mode, bindMain("orders_daily", "table-main"))
			f.resetRequests()
			out, err := runCLI(t, root, "wf", "push")
			assertSpaceRefused(t, f, out, err, "main.py:5 writes the table table-main (main_orders)", "in feature collection-main, which is in Space Main")
		})
	}
}

func TestSpaceRuleRefusesARawOutputLiteralIntoAnotherSpace(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode, func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "workflows/sales-report")
			asSpaceStack(t, root, mode, nil)
			appendLine(t, filepath.Join(root, "main.py"), `tools.appendToTable(fx, "table-main")`)
			f.resetRequests()
			out, err := runCLI(t, root, "wf", "push")
			assertSpaceRefused(t, f, out, err, "main.py:7 (tools.appendToTable) writes the table table-main", "Space Main")
		})
	}
}

// A workflow dependency not declared "run": true is still run.
func TestSpaceRuleRefusesAPlainWorkflowDependencyBoundIntoAnotherSpace(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode, func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "workflows/sales-report")
			asSpaceStack(t, root, mode, func(m *wfdir.Manifest) {
				m.Dependencies["closer"] = wfdir.Dependency{Kind: markers.KindWorkflow}
				m.Stacks["main"].Bind["closer"] = "workflow-main"
			})
			appendLine(t, filepath.Join(root, "main.py"), `done = {{ workflow('closer') }}`)
			f.resetRequests()
			out, err := runCLI(t, root, "wf", "push")
			assertSpaceRefused(t, f, out, err, "main.py:7 runs the workflow workflow-main (Main close)", "Space Main")
		})
	}
}

// A folder write scope lands in the default Space whatever the stack's Space,
// so a stack outside it may not write one; a stack in it may.
func TestSpaceRuleRefusesFolderWritesOutsideTheDefaultSpace(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode, func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "workflows/sales-report")
			asSpaceStack(t, root, mode, nil)
			appendLine(t, filepath.Join(root, "main.py"), `tools.saveFile(b, "x.csv", folder="/exports/daily")`)
			f.resetRequests()
			out, err := runCLI(t, root, "wf", "push")
			assertSpaceRefused(t, f, out, err, "main.py:7 (tools.saveFile) writes into the folder /exports/daily",
				"a run's file writes land in the organization's default Space, not in Space Dev", "write a table in the stack's own feature instead")

			for _, feature := range []string{importFeature, financeFeature} {
				f.featureSpace[feature] = "space-main"
			}
			m := loadAnyManifest(t, root)
			st := m.Stacks["main"]
			st.SpaceID, st.Space = "space-main", "Main"
			m.Stacks["main"] = st
			if err := wfdir.SaveManifest(root, m); err != nil {
				t.Fatal(err)
			}
			if out, err := runCLI(t, root, "wf", "push"); err != nil {
				t.Fatalf("a stack in the default Space was refused a folder write: %v\n%s", err, out)
			}
		})
	}
}

func TestSpaceRuleRefusesAnAppCodexInAnotherSpace(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode, func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "apps/sales-dashboard")
			asSpaceStack(t, root, mode, func(m *wfdir.Manifest) {
				access := m.DeclaredAccess()
				access.AllowedCodexIDs = []string{"cdx-main"}
				m.Access = &access
			})
			f.resetRequests()
			out, err := runCLI(t, root, "app", "push")
			assertSpaceRefused(t, f, out, err, "access.allowedCodexIDs[0] writes the codex cdx-main", "Space Main")
		})
	}
}

// withAutomationRef adds one entry to the sales assistant's reference set.
func withAutomationRef(t *testing.T, root, kind, id string) {
	t.Helper()
	path := filepath.Join(root, "sales-assistant.json")
	original := readFile(t, path)
	body := strings.Replace(original, `"references": [`, `"references": [`+"\n"+`    {"kind": "`+kind+`", "resourceID": "`+id+`"},`, 1)
	if body == original {
		t.Fatalf("no references list in:\n%s", original)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSpaceRuleRefusesAnAutomationReferenceInAnotherSpace(t *testing.T) {
	for _, mode := range spaceGateModes {
		for _, tc := range []struct{ kind, id, want string }{
			{"model", "table-main", "sales-assistant.json → references[0] writes the table table-main (main_orders)"},
			{"workflow", "workflow-main", "sales-assistant.json → references[0] runs the workflow workflow-main (Main close)"},
		} {
			t.Run(mode+"/"+tc.kind, func(t *testing.T) {
				f, dir := spacedTree(t)
				root := filepath.Join(dir, "automations/sales")
				asSpaceStack(t, root, mode, nil)
				withAutomationRef(t, root, tc.kind, tc.id)
				f.resetRequests()
				out, err := runCLI(t, root, "automation", "push")
				assertSpaceRefused(t, f, out, err, tc.want, "Space Main")
			})
		}
	}
}

func TestSpaceRuleRefusesADocsSidecarOnATableInAnotherSpace(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode, func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "pipelines/sales")
			asSpaceStack(t, root, mode, func(m *wfdir.Manifest) {
				m.Dependencies["main_orders"] = wfdir.Dependency{Kind: markers.KindTable}
				m.Stacks["main"].Bind["main_orders"] = "table-main"
			})
			writeSidecar(t, root, "main_orders", `{"description": "Orders, from the repository."}`)
			f.resetRequests()
			out, err := runCLI(t, root, "pipeline", "push")
			assertSpaceRefused(t, f, out, err, "documents the table table-main (main_orders)", "Space Main")
		})
	}
}

// `sync check` places what the code writes as the deploy would, and a dry run
// of `sync apply` refuses the folder on its line.
func TestSyncCheckAndDryRunApplyTheSpaceRule(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode, func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "workflows/sales-report")
			asSpaceStack(t, root, mode, bindMain("orders_daily", "table-main"))
			f.resetRequests()

			out, _ := runCLI(t, dir, "sync", "check", "--stack", "main", "--json")
			var report syncCheckReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("decode %q: %v", out, err)
			}
			var line *syncCheckFolderReport
			for i := range report.Folders {
				if report.Folders[i].Path == "workflows/sales-report" {
					line = &report.Folders[i]
				}
			}
			if line == nil || line.Verdict != verdictBroken || line.Reason != syncReasonBindSpaceMismatch || !strings.Contains(line.Detail, "table-main") {
				t.Fatalf("sync check: %+v", line)
			}

			apply, _ := applyStack(t, dir, "main", "--dry-run")
			got := applyFolderLine(t, &apply, "workflows/sales-report")
			if got.Verdict != verdictRefused || got.Reason != syncReasonBindSpaceMismatch {
				t.Fatalf("dry run: %s / %s — %s", got.Verdict, got.Reason, got.Detail)
			}
			if writes := workflowWrites(f); len(writes) != 0 {
				t.Fatalf("check and dry run sent %v", writes)
			}
		})
	}
}

// An address resolves within one Space. The pipeline's stack declares no
// Space, so it is placed by the rows the address resolves to, and a row in
// another Space is refused even though this folder only reads it.
func TestAnAddressIntoAnotherSpaceIsRefusedEvenForARead(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode, func(t *testing.T) {
			f, dir := spacedTree(t)
			f.pipe.AddFeature("collection-dev", "Sales (Dev)", "organization")
			f.featureSpace[importFeature], f.featureSpace["collection-dev"] = "space-main", "space-dev"
			root := filepath.Join(dir, "workflows/sales-report")
			asSpaceStack(t, root, mode, func(m *wfdir.Manifest) {
				st := m.Stacks["main"]
				st.FeatureID = "collection-dev"
				m.Stacks["main"] = st
			})
			f.resetRequests()
			out, err := runCLI(t, root, "wf", "push")
			if err == nil {
				t.Fatalf("the push went through:\n%s", out)
			}
			for _, want := range []string{`../../pipelines/sales at "orders_clean.sql" resolves in stack "main" to table-orders-clean, which is in Space Main`,
				"this stack is declared for Space Dev", "bind it by id"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if writes := workflowWrites(f); len(writes) != 0 {
				t.Fatalf("a refused push sent %v", writes)
			}
			check, _ := runCLI(t, dir, "sync", "check", "--stack", "main", "--json")
			if !strings.Contains(check, `"reason": "`+syncReasonAddressSpaceMismatch+`"`) {
				t.Fatalf("sync check does not report %s:\n%s", syncReasonAddressSpaceMismatch, check)
			}
		})
	}
}

// Each Space has its own secrets: a secret bound, or named by id, in another
// Space is refused, though the folder only reads it.
func TestASecretInAnotherSpaceIsRefused(t *testing.T) {
	for _, mode := range spaceGateModes {
		t.Run(mode+"/bound", func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "workflows/sales-report")
			var alias string
			asSpaceStack(t, root, mode, func(m *wfdir.Manifest) {
				for name, dep := range m.Dependencies {
					if dep.Kind == markers.KindSecret {
						alias = name
						m.Stacks["main"].Bind[name] = "secret-main"
					}
				}
			})
			if alias == "" {
				t.Fatal("the imported workflow declares no secret")
			}
			f.resetRequests()
			out, err := runCLI(t, root, "wf", "push")
			assertSpaceRefused(t, f, out, err, `"`+alias+`" is used by this folder and names secret-main in Space Main`)
		})
		t.Run(mode+"/by id", func(t *testing.T) {
			f, dir := spacedTree(t)
			root := filepath.Join(dir, "workflows/sales-report")
			asSpaceStack(t, root, mode, nil)
			appendLine(t, filepath.Join(root, "main.py"), `key = "{{ secret('secret-main', 'token') }}"`)
			f.resetRequests()
			out, err := runCLI(t, root, "wf", "push")
			assertSpaceRefused(t, f, out, err, "main.py uses the secret secret-main in Space Main")
		})
	}
}

// A folder refused by the Space rule is counted as refused, and the closing
// error names the refusal rather than references that do not resolve.
func TestSyncCheckNamesASpaceRefusalAsRefused(t *testing.T) {
	_, dir := spacedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	asSpaceStack(t, root, "unmanaged", bindMain("orders_daily", "table-main"))

	out, err := runCLI(t, root, "sync", "check", "--stack", "main")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit %d, want %d: %v\n%s", code, syncExitDrifted, err, out)
	}
	if strings.Contains(err.Error(), "do not resolve") || strings.Contains(err.Error(), "0 unresolved, 0 unreachable") {
		t.Fatalf("the error reads as unresolved references: %v", err)
	}
	if !strings.Contains(err.Error(), "1 folder refused (bind_space_mismatch)") {
		t.Fatalf("the error does not name the refusal: %v", err)
	}
	if !strings.Contains(out, "1 folder refused (bind_space_mismatch)") {
		t.Fatalf("the summary does not count the refusal:\n%s", out)
	}
}

// A tree with a refused folder and a folder whose references do not resolve
// reads as both.
func TestSyncCheckReadsAMixedTreeAsRefusedAndUnresolved(t *testing.T) {
	_, dir := spacedTree(t)
	asSpaceStack(t, filepath.Join(dir, "workflows/sales-report"), "unmanaged", bindMain("orders_daily", "table-main"))

	_, err := runCLI(t, dir, "sync", "check", "--stack", "main")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit %d, want %d: %v", code, syncExitDrifted, err)
	}
	want := "1 folder refused (bind_space_mismatch); 1 of 5 folders makes references that do not resolve (2 unresolved, 0 unreachable)"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v\nwant it to say %q", err, want)
	}
}

// --json counts the refused folder in summary.refused, and the exit code stays 1.
func TestSyncCheckJSONCountsARefusedFolder(t *testing.T) {
	_, dir := spacedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	asSpaceStack(t, root, "unmanaged", bindMain("orders_daily", "table-main"))

	out, err := runCLI(t, root, "sync", "check", "--stack", "main", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit %d, want %d: %v", code, syncExitDrifted, err)
	}
	var report struct {
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if report.Summary["refused"] != 1 || report.Summary["broken"] != 1 {
		t.Fatalf("summary = %v, want refused 1 of broken 1", report.Summary)
	}
}
