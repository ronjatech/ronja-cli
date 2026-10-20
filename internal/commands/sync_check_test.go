package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// seedPipelineFolder binds through a named stack because a bind map lives on one.
func seedPipelineFolder(t *testing.T, f *fakeInstance, dir string,
	deps map[string]wfdir.Dependency, bind map[string]string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: filepath.Base(dir),
		Dependencies: deps,
		Stacks: map[string]wfdir.Stack{
			"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1", Bind: bind},
		},
	}
	if err := wfdir.SaveFolder(dir, manifest, &wfdir.Lock{}); err != nil {
		t.Fatalf("save %s: %v", dir, err)
	}
	for path, content := range files {
		writeLocal(t, dir, path, content)
	}
	return dir
}

func seedAppFolder(t *testing.T, f *fakeInstance, dir string,
	access api.DataAppAccess, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindDataApp, Title: filepath.Base(dir), Entrypoint: "App.tsx",
		Stacks: map[string]wfdir.Stack{
			"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
		},
	}
	manifest.SetAccess(access)
	if err := wfdir.SaveFolder(dir, manifest, &wfdir.Lock{}); err != nil {
		t.Fatalf("save %s: %v", dir, err)
	}
	for path, content := range files {
		writeLocal(t, dir, path, content)
	}
	return dir
}

func checkFolders(t *testing.T, out string) []map[string]any {
	t.Helper()
	raw, ok := decodeJSON(t, out)["folders"].([]any)
	if !ok {
		t.Fatalf("no folders in payload:\n%s", out)
	}
	folders := make([]map[string]any, 0, len(raw))
	for _, folder := range raw {
		folders = append(folders, folder.(map[string]any))
	}
	return folders
}

func edgesOf(t *testing.T, folder map[string]any) []map[string]any {
	t.Helper()
	raw, ok := folder["edges"].([]any)
	if !ok {
		t.Fatalf("no edges on folder %v", folder["path"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, edge := range raw {
		out = append(out, edge.(map[string]any))
	}
	return out
}

func findEdge(t *testing.T, folder map[string]any, kind, ref string) map[string]any {
	t.Helper()
	for _, edge := range edgesOf(t, folder) {
		if edge["kind"] == kind && edge["ref"] == ref {
			return edge
		}
	}
	t.Fatalf("no %s reference to %q in %v", kind, ref, folder["edges"])
	return nil
}

func requestsTo(f *fakeInstance, fragment string) int {
	n := 0
	for _, request := range f.Requests {
		if strings.Contains(request, fragment) {
			n++
		}
	}
	return n
}

func TestSyncCheckCleanWorkflowTreeIsZero(t *testing.T) {
	f, tree, _ := clonedTree(t)

	out, err := runCLI(t, tree, "sync", "check", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	if verdict := decodeJSON(t, out)["verdict"]; verdict != verdictClean {
		t.Fatalf("verdict = %v, want %q\n%s", verdict, verdictClean, out)
	}
	if n := requestsTo(f, "/workflow/validate"); n != 1 {
		t.Fatalf("validate was called %d times, want 1 — a clean verdict that asked nothing is worse than no check", n)
	}
}

// The human report is the half a --json-only suite never exercises.
func TestSyncCheckHumanReportNamesTheProblem(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-dead"] = "Legacy"
	signIn(t, f)
	tree := t.TempDir()
	seedAppFolder(t, f, filepath.Join(tree, "app"),
		api.DataAppAccess{AllowedTableIDs: []string{"table-dead", "table-gone"}},
		map[string]string{"App.tsx": "const q = \"{{ ref('orders') }}\";\n"})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev")
	if exitCodeOf(err) == syncExitClean {
		t.Fatalf("expected a non-zero verdict for this fixture\n%s", out)
	}
	// Asserted on edge lines: the summary line prints every verdict word
	// unconditionally, so a grep over the whole report cannot fail.
	for _, want := range []string{
		"unresolved   table orders",
		"unreachable  table table-gone",
		"unused       table table-dead",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q line in the report:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "app (dataapp)") {
		t.Errorf("the folder listing does not name the folder and its kind:\n%s", out)
	}
}

// A deleted row's table.ErrNoRows arrives as a 400, not the no-enumeration 404;
// a verifier keyed on 404 alone would say nothing about a deleted table.
func TestSyncCheckUnreachableOn400AndOn404(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFakeInstance(t)
			f.missStatus = status
			signIn(t, f)
			tree := t.TempDir()
			seedPipelineFolder(t, f, filepath.Join(tree, "pipe"), nil, nil, map[string]string{
				"revenue.sql": "SELECT * FROM {{ ref('table-gone') }}\n",
			})

			out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
			if code := exitCodeOf(err); code != syncExitDrifted {
				t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
			}
			edge := findEdge(t, checkFolders(t, out)[0], markers.KindTable, "table-gone")
			if edge["verdict"] != edgeUnreachable {
				t.Fatalf("verdict = %v, want %q\n%s", edge["verdict"], edgeUnreachable, out)
			}
		})
	}
}

// The local id-shape pre-check is what makes the 400/404 collapse above safe.
func TestSyncCheckUnresolvedNameCostsNoRequest(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"), nil, nil, map[string]string{
		"revenue.sql": "SELECT * FROM {{ ref('orders') }}\n",
	})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	edge := findEdge(t, checkFolders(t, out)[0], markers.KindTable, "orders")
	if edge["verdict"] != edgeUnresolved {
		t.Fatalf("verdict = %v, want %q\n%s", edge["verdict"], edgeUnresolved, out)
	}
	if n := requestsTo(f, "/feature/model/"); n != 0 {
		t.Errorf("%d table lookups for a name that is not an id — the shape pre-check did not run", n)
	}
}

// Signed out, because that is where a CI job with a rotated token lands.
func TestSyncCheckUnboundDependencyIsFoundSignedOut(t *testing.T) {
	f := newFakeInstance(t)
	signOut(t, f)
	tree := t.TempDir()
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"),
		map[string]wfdir.Dependency{"orders": {Kind: markers.KindTable}}, nil,
		map[string]string{"revenue.sql": "SELECT * FROM {{ ref('orders') }}\n"})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	// Any non-zero exit; the finding must be in the payload.
	if exitCodeOf(err) == syncExitClean {
		t.Fatalf("a folder with an unbound dependency reported clean\n%s", out)
	}
	edges := edgesOf(t, checkFolders(t, out)[0])
	var found bool
	for _, edge := range edges {
		if edge["kind"] == "dependency" && edge["verdict"] == edgeUnresolved &&
			strings.Contains(edge["detail"].(string), `"orders"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unbound dependency was not reported:\n%s", out)
	}
}

// With no --stack, an unbound alias on a stack for this organization is
// unresolved, but when every stack names another organization nothing knows the
// target: that falls through to a nil-Bind Selection, which looks identical
// inside checkAliases and must be not_checked instead.
func TestSyncCheckUnboundAliasNeedsAResolvedStack(t *testing.T) {
	tests := []struct {
		name     string
		tenantID string
		wantExit int
		wantEdge string
	}{
		{"this organization", testTenantID, syncExitDrifted, edgeUnresolved},
		{"another organization", "ten-other", syncExitUnknown, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeInstance(t)
			signIn(t, f)
			tree := t.TempDir()
			dir := filepath.Join(tree, "pipe")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := &wfdir.Manifest{
				Kind: wfdir.KindPipeline, Title: "Ops",
				Dependencies: map[string]wfdir.Dependency{"orders": {Kind: markers.KindTable}},
				// Declared, and bound to NOTHING.
				Stacks: map[string]wfdir.Stack{
					"prod": {URL: f.URL(), TenantID: test.tenantID, FeatureID: "feat-1"},
				},
			}
			if err := wfdir.SaveFolder(dir, manifest, &wfdir.Lock{}); err != nil {
				t.Fatal(err)
			}
			writeLocal(t, dir, "revenue.sql", "SELECT * FROM {{ ref('orders') }}\n")

			out, err := runCLI(t, tree, "sync", "check", "--json")
			if code := exitCodeOf(err); code != test.wantExit {
				t.Fatalf("exit = %d (%v), want %d\n%s", code, err, test.wantExit, out)
			}
			folder := checkFolders(t, out)[0]
			if test.wantEdge == "" {
				if folder["reason"] != syncReasonNotBoundHere {
					t.Fatalf("reason = %v, want %q\n%s", folder["reason"], syncReasonNotBoundHere, out)
				}
				detail, _ := folder["detail"].(string)
				if strings.Contains(detail, "instances") {
					t.Errorf("the detail names an \"instances\" entry this manifest does not have: %s", detail)
				}
				if !strings.Contains(detail, "prod") {
					t.Errorf("the detail does not name the stack the folder declares: %s", detail)
				}
				return
			}
			edge := findEdge(t, folder, "dependency", "orders")
			if edge["verdict"] != test.wantEdge {
				t.Fatalf("verdict = %v, want %q\n%s", edge["verdict"], test.wantEdge, out)
			}
		})
	}
}

// The codex route group declares no access scope, so every scoped token is
// refused, and there is no read-one-mailbox route: neither kind is verifiable.
func TestSyncCheckCodexAndMailboxAreNotChecked(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"), nil, nil, map[string]string{
		"revenue.sql": "-- {{ codex('cdx-1') }} {{ mailbox('mailbox-1') }}\nSELECT 1\n",
	})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	folder := checkFolders(t, out)[0]
	for _, want := range []struct{ kind, ref, reason string }{
		{markers.KindCodex, "cdx-1", syncReasonCodexUnverifiable},
		{markers.KindMailbox, "mailbox-1", syncReasonMailboxUnverifiable},
	} {
		edge := findEdge(t, folder, want.kind, want.ref)
		if edge["verdict"] != edgeNotChecked {
			t.Errorf("%s verdict = %v, want %q", want.kind, edge["verdict"], edgeNotChecked)
		}
		if edge["reason"] != want.reason {
			t.Errorf("%s reason = %v, want %q", want.kind, edge["reason"], want.reason)
		}
	}
	if n := requestsTo(f, "/codex/") + requestsTo(f, "/mailbox/"); n != 0 {
		t.Errorf("%d requests for kinds no scoped token can read", n)
	}
}

// A stack with no featureID is normal before a first push, and validate refuses
// it: not_checked, since broken would fail every new folder.
func TestSyncCheckWorkflowWithoutAFeatureIsNotChecked(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	dir := filepath.Join(tree, "wf")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Monthly report", Entrypoint: "main.py",
		// A stack with no featureID: bound to the organization, never pushed.
		Stacks: map[string]wfdir.Stack{"dev": {URL: f.URL(), TenantID: testTenantID}},
	}
	if err := wfdir.SaveFolder(dir, manifest, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, dir, "main.py", "print('hi')\n")

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	folder := checkFolders(t, out)[0]
	if folder["verdict"] != verdictUnknown || folder["reason"] != syncReasonNoFeature {
		t.Fatalf("folder = %v, want %s/%s", folder, verdictUnknown, syncReasonNoFeature)
	}
	if n := requestsTo(f, "/workflow/validate"); n != 0 {
		t.Errorf("validate was called %d times for a stack with no feature — it refuses those before doing any work", n)
	}
}

// The dependency leg needs no server, so its finding must survive a
// folder-level could-not-check.
func TestSyncCheckKeepsLocalFindingsWhenTheFolderCannotBeChecked(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	dir := filepath.Join(tree, "wf")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Monthly report", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{"orders": {Kind: markers.KindTable}},
		// No featureID, so validate cannot be asked at all.
		Stacks: map[string]wfdir.Stack{"dev": {URL: f.URL(), TenantID: testTenantID}},
	}
	if err := wfdir.SaveFolder(dir, manifest, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, dir, "main.py", "read(\"{{ ref('orders') }}\")\n")

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	folder := checkFolders(t, out)[0]
	if folder["reason"] != syncReasonNoFeature {
		t.Fatalf("reason = %v, want %q\n%s", folder["reason"], syncReasonNoFeature, out)
	}
	if edges := edgesOf(t, folder); len(edges) == 0 {
		t.Fatalf("the unbound dependency was dropped along with the folder:\n%s", out)
	}
}

// secret_dropped is a warning server-side (a save must not wait on a credential
// about to be connected), but here it is unreachable: the workflow runs with
// nothing bound.
func TestSyncCheckMapsValidateFindings(t *testing.T) {
	tests := []struct {
		name     string
		finding  api.ValidateFinding
		wantExit int
		wantEdge string
	}{
		{"unresolved ref", api.ValidateFinding{
			Severity: "error", Code: "unresolved_ref", Message: "no such table", Path: "main.py",
			Marker: "{{ ref('table-gone') }}",
		}, syncExitDrifted, edgeUnreachable},
		{"dropped secret", api.ValidateFinding{
			Severity: "warning", Code: "secret_dropped", Message: "secret not reachable", Path: "main.py",
			Marker: "{{ secret('secret-gone', 'token') }}",
		}, syncExitDrifted, edgeUnreachable},
		// An unmapped code would still redden the verdict via findings; this
		// pins that a module arrives as an edge, scannable by name.
		{"unresolved module", api.ValidateFinding{
			Severity: "error", Code: "unresolved_module", Message: "no such module", Path: "main.py",
			Marker: "{{ module('module-gone') }}",
		}, syncExitDrifted, edgeUnreachable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, tree, _ := clonedTree(t)
			f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{test.finding}}

			out, err := runCLI(t, tree, "sync", "check", "--json")
			if code := exitCodeOf(err); code != test.wantExit {
				t.Fatalf("exit = %d (%v), want %d\n%s", code, err, test.wantExit, out)
			}
			edges := edgesOf(t, checkFolders(t, out)[0])
			if len(edges) != 1 || edges[0]["verdict"] != test.wantEdge {
				t.Fatalf("edges = %v, want one %q\n%s", edges, test.wantEdge, out)
			}
		})
	}
}

// ⚠️ --allow-dropped-bindings must change the score and never the report.
func TestSyncCheckAllowDroppedBindings(t *testing.T) {
	dropped := api.ValidateFinding{
		Severity: "warning", Code: "secret_dropped", Message: "secret not reachable", Path: "main.py",
		Marker: "{{ secret('secret-gone', 'token') }}",
	}
	tests := []struct {
		name     string
		args     []string
		wantExit int
	}{
		{"refused by default", []string{"sync", "check", "--json"}, syncExitDrifted},
		{"accepted with the flag", []string{"sync", "check", "--json", "--allow-dropped-bindings"}, syncExitClean},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, tree, _ := clonedTree(t)
			f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{dropped}}

			out, err := runCLI(t, tree, test.args...)
			if code := exitCodeOf(err); code != test.wantExit {
				t.Fatalf("exit = %d (%v), want %d\n%s", code, err, test.wantExit, out)
			}
			edge := findEdge(t, checkFolders(t, out)[0], markers.KindSecret, "secret-gone")
			if edge["verdict"] != edgeUnreachable {
				t.Errorf("verdict = %v, want %q — the flag changed the report, not just the score", edge["verdict"], edgeUnreachable)
			}
			if edge["droppedBinding"] != true {
				t.Errorf("the edge is not marked as a dropped binding: %v", edge)
			}
			if n := decodeJSON(t, out)["summary"].(map[string]any)["droppedBindings"]; n != float64(1) {
				t.Errorf("summary droppedBindings = %v, want 1", n)
			}
		})
	}
}

func TestSyncCheckNamesTheDroppedBindingsFlag(t *testing.T) {
	f, tree, _ := clonedTree(t)
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: "warning", Code: "secret_dropped", Message: "secret not reachable", Path: "main.py",
		Marker: "{{ secret('secret-gone', 'token') }}",
	}}}

	_, err := runCLI(t, tree, "sync", "check", "--json")
	if err == nil || !strings.Contains(err.Error(), "--allow-dropped-bindings") {
		t.Fatalf("the refusal does not name the flag: %v", err)
	}
}

func TestSyncCheckReportsNonReferenceFindings(t *testing.T) {
	f, tree, _ := clonedTree(t)
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: "error", Code: "mixed_ref_style", Message: "positional and id refs in one file", Path: "main.py",
	}}}

	out, err := runCLI(t, tree, "sync", "check", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	folder := checkFolders(t, out)[0]
	findings, ok := folder["findings"].([]any)
	if !ok || len(findings) != 1 || !strings.Contains(findings[0].(string), "mixed_ref_style") {
		t.Fatalf("findings = %v\n%s", folder["findings"], out)
	}
}

// Used-but-undeclared is the half that matters: unlike the agent path, the HTTP
// push route never auto-repairs the allowlist, so such an app fails only at run
// time with rejected_access.
func TestSyncCheckDataAppComparesDeclaredAndUsed(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-used"] = "Orders"
	f.tableNames["table-dead"] = "Legacy"
	signIn(t, f)
	tree := t.TempDir()
	seedAppFolder(t, f, filepath.Join(tree, "app"),
		api.DataAppAccess{AllowedTableIDs: []string{"table-used", "table-dead"}},
		map[string]string{
			"App.tsx": "const q = \"{{ ref('table-used') }}\";\nconst other = \"table-undeclared\";\n",
		})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	folder := checkFolders(t, out)[0]

	findings, _ := folder["findings"].([]any)
	if len(findings) != 1 || !strings.Contains(findings[0].(string), "table-undeclared") {
		t.Errorf("used-but-undeclared was not reported: %v\n%s", folder["findings"], out)
	}
	// ⚠️ The `access` block is the app's privileges: advice that only widens the
	// allowlist turns every false positive into an unintended grant.
	if !strings.Contains(findings[0].(string), "delete the reference") {
		t.Errorf("the finding advertises widening without offering the other remedy: %v", findings[0])
	}
	if dead := findEdge(t, folder, markers.KindTable, "table-dead"); dead["unused"] != true {
		t.Errorf("declared-but-unused was not reported: %v", dead)
	}
	if used := findEdge(t, folder, markers.KindTable, "table-used"); used["unused"] == true {
		t.Errorf("a declared id the source uses was reported as unused: %v", used)
	}
}

// A positional ref indexes the row's own input_models on the server, so the
// file cannot say what it points at.
func TestSyncCheckPositionalRefIsNotChecked(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"), nil, nil, map[string]string{
		"revenue.sql": "SELECT * FROM {{ ref('0') }}\n",
	})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	edge := findEdge(t, checkFolders(t, out)[0], markers.KindTable, "0")
	if edge["verdict"] != edgeNotChecked || edge["reason"] != syncReasonPositionalRef {
		t.Fatalf("edge = %v, want %s/%s", edge, edgeNotChecked, syncReasonPositionalRef)
	}
}

// Without a managed `access` block the allowlist lives in Ronja, so there is
// nothing declared to compare against: never green.
func TestSyncCheckUnmanagedAccessIsNotChecked(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	dir := filepath.Join(tree, "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindDataApp, Title: "Revenue explorer", Entrypoint: "App.tsx",
		Stacks: map[string]wfdir.Stack{"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"}},
	}
	if err := wfdir.SaveFolder(dir, manifest, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, dir, "App.tsx", "const x = 1;\n")

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	edges := edgesOf(t, checkFolders(t, out)[0])
	if len(edges) != 1 || edges[0]["reason"] != syncReasonAccessUnmanaged {
		t.Fatalf("edges = %v, want one %q\n%s", edges, syncReasonAccessUnmanaged, out)
	}
}

// ⚠️ An app folder syncs README, notes and fixtures too, and a false positive
// there invites granting the app a row it does not need; the scan covers only
// the files the bundle executes.
func TestSyncCheckIgnoresIDsOutsideAppSource(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-used"] = "Orders"
	signIn(t, f)
	tree := t.TempDir()
	seedAppFolder(t, f, filepath.Join(tree, "app"),
		api.DataAppAccess{AllowedTableIDs: []string{"table-used"}},
		map[string]string{
			"App.tsx":               "const q = \"{{ ref('table-used') }}\";\n",
			"README.md":             "Reads `table-documented` in production.\n",
			"docs/design.md":        "We used to read \"table-legacy\".\n",
			"testdata/fixture.json": "{\"tableID\": \"table-fixture\"}\n",
			"testdata/expected.csv": "id\ntable-in-a-csv\n",
		})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d — an id outside app source is not a reference\n%s",
			code, err, syncExitClean, out)
	}
	if findings := checkFolders(t, out)[0]["findings"]; findings != nil {
		t.Fatalf("findings from non-source files: %v\n%s", findings, out)
	}
}

func TestSyncCheckStillGatesOnIDsInAppSource(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	seedAppFolder(t, f, filepath.Join(tree, "app"), api.DataAppAccess{},
		map[string]string{
			"App.tsx":      "export default () => null;\n",
			"lib/query.ts": "const SECRET = \"secret-live\";\n",
		})

	_, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d — a quoted id in a .ts file is a reference", code, err, syncExitDrifted)
	}
}

func TestSyncCheckDedupesLookupsAcrossTheTree(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-shared"] = "Orders"
	signIn(t, f)
	tree := t.TempDir()
	for _, name := range []string{"one", "two"} {
		seedPipelineFolder(t, f, filepath.Join(tree, name), nil, nil, map[string]string{
			"a.sql": "SELECT * FROM {{ ref('table-shared') }}\n",
			"b.sql": "SELECT * FROM {{ ref('table-shared') }}\n",
		})
	}

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	if n := requestsTo(f, "/feature/model/table-shared"); n != 1 {
		t.Fatalf("%d lookups of one table id, want exactly 1 — the (kind, id) dedupe is not spanning the tree", n)
	}
}

func TestSyncCheckUnknownDominatesBroken(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"), nil, nil, map[string]string{
		"revenue.sql": "SELECT * FROM {{ ref('table-gone') }}\n-- {{ codex('cdx-1') }}\n",
	})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	if verdict := decodeJSON(t, out)["verdict"]; verdict != verdictUnknown {
		t.Fatalf("verdict = %v, want %q\n%s", verdict, verdictUnknown, out)
	}
}

// A renamed or moved directory must not sail through the gate.
func TestSyncCheckEmptyWalkIsUnknown(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "sync", "check", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d", code, err, syncExitUnknown)
	}
	if verdict := decodeJSON(t, out)["verdict"]; verdict != verdictUnknown {
		t.Fatalf("verdict = %v, want %q", verdict, verdictUnknown)
	}
}

func TestSyncCheckUnknownStackIsNeverGreen(t *testing.T) {
	_, tree, _ := clonedTree(t)

	out, err := runCLI(t, tree, "sync", "check", "--stack", "nope", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	folder := checkFolders(t, out)[0]
	if folder["verdict"] != verdictUnknown {
		t.Errorf("verdict = %v, want %q", folder["verdict"], verdictUnknown)
	}
}

// openFolder calls adoptStack, which rewrites ronja.json, so reusing the
// `ronja wf validate` path would write as a side effect of a check.
// seedLockAdoptedFolder is the fixture that actually reaches adoptStack.
func TestSyncCheckWritesNothing(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "print('hi')\n"},
	)
	signIn(t, f)
	tree := t.TempDir()
	seedLockAdoptedFolder(t, f, filepath.Join(tree, "adopts"))
	// A real reference, so the run does network work rather than bailing early.
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"), nil, nil, map[string]string{
		"revenue.sql": "SELECT * FROM {{ ref('table-gone') }}\n",
	})

	assertTreeUntouched(t, tree, func() {
		if _, err := runCLI(t, tree, "sync", "check", "--stack", "prod", "--json"); exitCodeOf(err) == 0 {
			t.Fatalf("expected a non-zero verdict for this fixture, got clean")
		}
	})
}

// The exit code must survive `run`, not only RunE: it is all a CI job sees.
func TestSyncCheckExitCodeReachesTheProcess(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	t.Chdir(t.TempDir())

	if code := run([]string{"sync", "check"}); code != syncExitUnknown {
		t.Fatalf("run exit = %d, want %d", code, syncExitUnknown)
	}
}

// Each kind on both sides of its route: a wrong route either reddens a build
// over a live row or greens one over a broken reference.
func TestSyncCheckVerifiesEveryKindByID(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-live"] = "Orders"
	f.agentIDs["agent-live"] = true
	f.secretIDs["secret-live"] = true
	f.AddWorkflow(&api.Workflow{ID: "workflow-live", Lifecycle: api.LifecycleLive}, api.WorkflowFile{Path: "main.py"})
	signIn(t, f)
	tree := t.TempDir()
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"), nil, nil, map[string]string{
		"revenue.sql": `-- {{ ref('table-live') }} {{ ref('table-gone') }}
-- {{ agent('agent-live') }} {{ agent('agent-gone') }}
-- {{ secret('secret-live') }} {{ secret('secret-gone') }}
-- {{ workflow('workflow-live') }} {{ workflow('workflow-gone') }}
SELECT 1
`,
	})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	folder := checkFolders(t, out)[0]
	for _, want := range []struct{ kind, ref, verdict string }{
		{markers.KindTable, "table-live", edgeOK},
		{markers.KindTable, "table-gone", edgeUnreachable},
		{markers.KindAgent, "agent-live", edgeOK},
		{markers.KindAgent, "agent-gone", edgeUnreachable},
		{markers.KindSecret, "secret-live", edgeOK},
		{markers.KindSecret, "secret-gone", edgeUnreachable},
		{markers.KindWorkflow, "workflow-live", edgeOK},
		{markers.KindWorkflow, "workflow-gone", edgeUnreachable},
	} {
		if got := findEdge(t, folder, want.kind, want.ref)["verdict"]; got != want.verdict {
			t.Errorf("%s %s = %v, want %q", want.kind, want.ref, got, want.verdict)
		}
	}
}

// A folder-level finding scores like a broken edge, so the refusal must count it.
func TestSyncCheckCountsFindingsInTheRefusal(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	seedAppFolder(t, f, filepath.Join(tree, "app"),
		api.DataAppAccess{},
		map[string]string{"App.tsx": "const q = \"table-undeclared\";\n"})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if n := decodeJSON(t, out)["summary"].(map[string]any)["findings"]; n != float64(1) {
		t.Errorf("summary findings = %v, want 1\n%s", n, out)
	}
	if err == nil || !strings.Contains(err.Error(), "1 other finding") {
		t.Errorf("the refusal does not account for the finding it scored: %v", err)
	}
}

func TestSyncCheckSignedOutStillReportsDependencies(t *testing.T) {
	f := newFakeInstance(t)
	signOut(t, f)
	tree := t.TempDir()
	dir := filepath.Join(tree, "wf")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Monthly report", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{"orders": {Kind: markers.KindTable}},
		Stacks: map[string]wfdir.Stack{
			"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
		},
	}
	if err := wfdir.SaveFolder(dir, manifest, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, dir, "main.py", "read(\"{{ ref('orders') }}\")\n")

	// No --stack: with one, this lands on stack_unverified (the next test).
	// Signed out, the stack still matches on the instance alone.
	out, err := runCLI(t, tree, "sync", "check", "--json")
	if exitCodeOf(err) == syncExitClean {
		t.Fatalf("a folder with an unbound dependency reported clean\n%s", out)
	}
	folder := checkFolders(t, out)[0]
	if folder["reason"] != syncReasonNoCredential {
		t.Fatalf("reason = %v, want %q\n%s", folder["reason"], syncReasonNoCredential, out)
	}
	edge := findEdge(t, folder, "dependency", "orders")
	if edge["verdict"] != edgeUnresolved {
		t.Errorf("verdict = %v, want %q", edge["verdict"], edgeUnresolved)
	}
}

// For an organization we cannot confirm is ours, every remote leg is unsafe (an
// id lookup would call another org's rows unreachable); the dependency leg is not.
func TestSyncCheckUnverifiedOrganizationStillReportsDependencies(t *testing.T) {
	f := newFakeInstance(t)
	f.failMe = 500
	signIn(t, f)
	tree := t.TempDir()
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"),
		map[string]wfdir.Dependency{"orders": {Kind: markers.KindTable}}, nil,
		map[string]string{"revenue.sql": "SELECT * FROM {{ ref('table-gone') }}\n"})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	folder := checkFolders(t, out)[0]
	if folder["reason"] != syncReasonStackUnverified {
		t.Fatalf("reason = %v, want %q\n%s", folder["reason"], syncReasonStackUnverified, out)
	}
	if edge := findEdge(t, folder, "dependency", "orders"); edge["verdict"] != edgeUnresolved {
		t.Errorf("the no-network leg was dropped: %v", edge)
	}
	if n := requestsTo(f, "/feature/model/"); n != 0 {
		t.Errorf("%d id lookups against an organization we could not confirm", n)
	}
}

// A kind added to markers must be covered by the literal scan without a second
// hand-kept list.
func TestSyncCheckLiteralScanCoversEveryMarkerPrefix(t *testing.T) {
	for _, prefix := range markers.IDPrefixes() {
		id := prefix + "orphan"
		kind := kindOfResourceID(id)
		if kind == "" {
			t.Fatalf("markers declares prefix %q and kindOfResourceID cannot name its kind", prefix)
		}
		t.Run(prefix, func(t *testing.T) {
			f := newFakeInstance(t)
			signIn(t, f)
			tree := t.TempDir()
			seedAppFolder(t, f, filepath.Join(tree, "app"), api.DataAppAccess{},
				map[string]string{"App.tsx": "const X = \"" + id + "\";\n"})

			out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
			if exitCodeOf(err) == syncExitClean {
				t.Fatalf("a quoted %s id in app source was not seen at all — the pattern does not cover %q\n%s",
					kind, prefix, out)
			}
			findings, _ := checkFolders(t, out)[0]["findings"].([]any)
			if len(findings) != 1 || !strings.Contains(findings[0].(string), id) {
				t.Fatalf("findings = %v, want one naming %s\n%s", findings, id, out)
			}
		})
	}
}
