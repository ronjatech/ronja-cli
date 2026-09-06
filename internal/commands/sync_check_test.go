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

// seedPipelineFolder writes a pipeline folder bound to a fake instance's stack,
// with whatever .sql files it is given.
//
// Bound through a NAMED stack because that is the shape the alias layer needs:
// a bind map lives on a stack, and the whole point of `sync check` is to notice
// when one of those binds is missing.
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

// seedAppFolder is seedPipelineFolder for a data app, with a declared `access`
// block.
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

// checkFolders decodes a `sync check --json` payload's folder list.
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

// edgesOf returns one folder's references.
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

// findEdge returns the first reference matching a kind and a written form.
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

// requestsTo counts the fake's requests whose path contains a fragment.
func requestsTo(f *fakeInstance, fragment string) int {
	n := 0
	for _, request := range f.Requests {
		if strings.Contains(request, fragment) {
			n++
		}
	}
	return n
}

// A folder whose every reference resolves is clean and exits zero. The workflow
// leg is the instance's own validate endpoint, so "clean" here means the server
// looked and found nothing.
func TestSyncCheckCleanWorkflowTreeIsZero(t *testing.T) {
	f, tree, _ := clonedTree(t)

	out, err := runCLI(t, tree, "sync", "check", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	if verdict := decodeJSON(t, out)["verdict"]; verdict != verdictClean {
		t.Fatalf("verdict = %v, want %q\n%s", verdict, verdictClean, out)
	}
	// Clean because the instance was ASKED and answered, not because the leg
	// quietly did nothing — which is how a check goes green while checking
	// nothing at all.
	if n := requestsTo(f, "/workflow/validate"); n != 1 {
		t.Fatalf("validate was called %d times, want 1 — a clean verdict that asked nothing is worse than no check", n)
	}
}

// The human report is what most people read, and it is the half a --json-only
// test suite never exercises: a nil map or a bad format verb there is a panic in
// front of a customer.
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
	// ⚠️ Asserted on the EDGE LINES, not on the whole report. The summary line
	// prints every verdict word unconditionally ("0 ok, 1 unresolved, 1
	// unreachable, …"), so a grep over the output passed even when every edge was
	// labelled something else entirely — the test could not fail.
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

// The vocabulary's whole point: an id the server will not show us is
// UNREACHABLE, and it is reported that way whether the refusal arrives as the
// no-enumeration 404 or as the 400 a deleted row's table.ErrNoRows produces.
//
// A verifier keyed on 404 alone would read a deleted table as "my request was
// malformed" and say nothing at all — which is the exact failure this command
// exists to prevent.
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

// The local id-shape pre-check is what makes the 400/404 collapse above safe: a
// name that was never an id must be `unresolved` — decided here, definitively —
// and must cost no request at all, because there is nothing to ask about.
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

// The leg that needs no network at all, and the failure the whole alias layer
// exists to fix: an alias this folder declares and this stack binds nothing to.
// It has to be reported signed OUT, because that is where a CI job with a
// rotated token lands and it is still the truth.
func TestSyncCheckUnboundDependencyIsFoundSignedOut(t *testing.T) {
	f := newFakeInstance(t)
	signOut(t, f)
	tree := t.TempDir()
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"),
		map[string]wfdir.Dependency{"orders": {Kind: markers.KindTable}}, nil,
		map[string]string{"revenue.sql": "SELECT * FROM {{ ref('orders') }}\n"})

	out, err := runCLI(t, tree, "sync", "check", "--stack", "dev", "--json")
	// Non-zero either way; what matters is that the refusal is IN the payload.
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

// The boundary that decides whether an unbound alias is a FINDING or a
// SHRUG, with NO --stack given. Both sides, because they run through the same
// code and the wrong answer on either is invisible from the message text alone.
//
// The folder that resolves an entry for this credential is the flagship case
// this whole project exists to catch: it has a target, its alias binds to
// nothing for that target, and that is `unresolved` / exit 1 with no network.
//
// The folder whose stacks all name ANOTHER organization is not the same
// statement at all. We do not know which organization it targets, so "this alias
// is unbound" is not something anything here is entitled to say — it falls
// through to an unbound Selection whose Bind is nil, which looks identical from
// inside checkAliases and is not. That is `not_checked` / exit 2.
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

			// No --stack, deliberately: that is the direction the named-stack rule
			// did not cover.
			out, err := runCLI(t, tree, "sync", "check", "--json")
			if code := exitCodeOf(err); code != test.wantExit {
				t.Fatalf("exit = %d (%v), want %d\n%s", code, err, test.wantExit, out)
			}
			folder := checkFolders(t, out)[0]
			if test.wantEdge == "" {
				if folder["reason"] != syncReasonNotBoundHere {
					t.Fatalf("reason = %v, want %q\n%s", folder["reason"], syncReasonNotBoundHere, out)
				}
				// The old message named an "instances" entry a stacks-only manifest
				// does not contain, which sent the reader after config that is not
				// there. It must name what the folder DOES declare.
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

// Codex and mailbox cannot be verified by id AT ALL — the codex route group
// declares no access scope so every scoped token is refused, and there is no
// read-one-mailbox route to ask. Both must say so rather than pass as ok, and
// neither may cost a request.
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

// POST /workflow/validate refuses without a featureID, and a stack that has
// none is the NORMAL state before a folder's first push — the onboarding case
// for a repository being adopted. It must be not_checked with a named reason:
// reporting it as broken would fail every new folder, and skipping it silently
// would report one as fine.
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

// A folder-level "could not check" does NOT throw away what was already
// decided. The dependency leg needs no server at all, so a folder whose remote
// half could not be reached must still report the alias its stack binds nothing
// to — otherwise the one finding that never needed the network disappears
// precisely when the network is what failed.
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

// The server's per-marker findings map onto the vocabulary, and `secret_dropped`
// is the one worth pinning: it is a WARNING server-side, because refusing a save
// over a credential somebody is about to connect would be obstructive. That is a
// policy about writing. This command asks whether every reference resolves, and
// a dropped secret is one that does not — the workflow runs with nothing bound.
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
		// `{{ module }}` is a per-reference marker like the rest, and an
		// unmapped code is not a silent drop — it lands in the folder's
		// `findings` and still reddens the verdict — so the thing this case
		// actually pins is that it arrives as an EDGE, which is the only shape
		// a reader can scan for the module by name.
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

// `--allow-dropped-bindings` is the hatch `ronja wf push` already has for the
// same finding: a `{{ secret }}` naming a credential nobody has created yet is
// the ordinary state of a repository mid-setup, and without it the only way to a
// green tree is not to run the command.
//
// ⚠️ It must change the SCORE and never the REPORT. A flag that hid the evidence
// would be the failure mode, so both directions assert the edge is still there,
// still `unreachable`, still marked as the drop it is.
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
			// The evidence survives in BOTH runs, unchanged.
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

// A hatch nobody can find is not a hatch: when a dropped binding is part of what
// pushed the run to exit 1, the refusal names the flag — the same thing
// `ronja wf push` does in the same place.
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

// A finding that is not about ONE reference — a file mixing positional and id
// refs — is a folder-level finding rather than an edge, and still fails the run.
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

// Both directions of the data app's declared-versus-used comparison.
//
// The used-but-undeclared half is the one that matters: the HTTP route the CLI
// pushes through calls UpsertFileChecked and nothing else, so unlike the agent
// path a CLI-pushed app's allowlist is NEVER auto-repaired. Such an app pushes
// clean, compiles, publishes, and then fails at run time with rejected_access.
func TestSyncCheckDataAppComparesDeclaredAndUsed(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-used"] = "Orders"
	f.tableNames["table-dead"] = "Legacy"
	signIn(t, f)
	tree := t.TempDir()
	seedAppFolder(t, f, filepath.Join(tree, "app"),
		api.DataAppAccess{AllowedTableIDs: []string{"table-used", "table-dead"}},
		map[string]string{
			// One declared id used, one declared id used by nothing, and one id
			// the source reaches for that nothing declares.
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
	// ⚠️ The `access` block is the app's PRIVILEGES, so the finding must not lead
	// with "add it". A message whose default advice is to widen an allowlist
	// turns every false positive into a grant nobody meant to make — and deleting
	// a leftover reference is the other half of the answer, not a footnote.
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

// A positional `{{ ref('0') }}` indexes the table row's OWN input_models on the
// server, so what it points at is a property of the row rather than of the file
// on disk. Reported as not checked rather than skipped: an edge nobody looked at
// must not read as an edge that is fine.
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

// A data-app folder that does not manage its `access` block has nothing
// declared to compare its source against — the allowlist lives in Ronja. Said
// out loud, and never green: an app whose grants this folder does not own is one
// this command cannot fully vouch for.
func TestSyncCheckUnmanagedAccessIsNotChecked(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	dir := filepath.Join(tree, "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No SetAccess: the folder declares no allowlist at all.
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

// wfdir.DataAppKind declares NO SyncExt, so an app folder syncs everything the
// structural rules allow — README, design notes, test fixtures. A quoted
// `table-…` in any of those used to fail the whole tree, and the remedy the
// finding advertised was to add that id to the app's real `access` allowlist.
//
// ⚠️ That is a security regression produced by a false positive, which is worse
// than the miss it prevents: a red gate with a one-line fix that quietly grants
// the app a row it does not need. The scan is scoped to the files the bundle
// actually executes.
func TestSyncCheckIgnoresIDsOutsideAppSource(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-used"] = "Orders"
	signIn(t, f)
	tree := t.TempDir()
	seedAppFolder(t, f, filepath.Join(tree, "app"),
		api.DataAppAccess{AllowedTableIDs: []string{"table-used"}},
		map[string]string{
			"App.tsx": "const q = \"{{ ref('table-used') }}\";\n",
			// Every shape of committed non-source file that mentions an id.
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

// The other side of the same boundary: inside real app source, a quoted id IS
// very likely a real reference, and that is the break worth catching.
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

// Ids repeat heavily across a feature's folders, so verification is deduped by
// (kind, id) across the WHOLE tree — one row read, however many folders read it.
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
	// Four references to one row, across two folders, in one process.
	if n := requestsTo(f, "/feature/model/table-shared"); n != 1 {
		t.Fatalf("%d lookups of one table id, want exactly 1 — the (kind, id) dedupe is not spanning the tree", n)
	}
}

// "Could not tell" wins over a finding, exactly as it does for `sync status`:
// the strongest true statement about a folder where one reference is broken and
// another could not be looked at is that not everything was verified.
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

// A renamed or moved directory must not sail through the gate reporting
// success. Zero folders is exit 2, never 0 — the same rule `sync status` has.
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

// A folder that does not declare the stack asked for is skipped rather than
// aborting the run, and is never counted as clean — the same rule, and the same
// walk, `sync status` uses.
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

// `ronja sync check` must not touch the customer's repository. AT ALL.
//
// It matters MORE here than for `sync status`: the obvious way to reuse
// `ronja wf validate` is to open the folder the way that command does, and
// openFolder calls adoptStack, which REWRITES ronja.json. A tree command doing
// that in forty folders as a side effect of a check is the exact failure this
// pins. wfdir.SaveState also writes a .gitignore.
//
// ⚠️ This test used to stage a legacy instances[] folder and then run with NO
// --stack — a fixture adoptStack returns from immediately, so the test was green
// whether or not the command wrote. seedLockAdoptedFolder is the shape that
// really reaches it; see its own note for why it is the only one that does.
func TestSyncCheckWritesNothing(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "print('hi')\n"},
	)
	signIn(t, f)
	tree := t.TempDir()
	seedLockAdoptedFolder(t, f, filepath.Join(tree, "adopts"))
	// A pipeline folder with a real reference, so the run does network work
	// rather than bailing early on every folder it looks at.
	seedPipelineFolder(t, f, filepath.Join(tree, "pipe"), nil, nil, map[string]string{
		"revenue.sql": "SELECT * FROM {{ ref('table-gone') }}\n",
	})

	assertTreeUntouched(t, tree, func() {
		if _, err := runCLI(t, tree, "sync", "check", "--stack", "prod", "--json"); exitCodeOf(err) == 0 {
			t.Fatalf("expected a non-zero verdict for this fixture, got clean")
		}
	})
}

// The exit code has to survive `run`, not only the RunE error, because that is
// the only thing a CI job actually sees.
func TestSyncCheckExitCodeReachesTheProcess(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	t.Chdir(t.TempDir())

	if code := run([]string{"sync", "check"}); code != syncExitUnknown {
		t.Fatalf("run exit = %d, want %d", code, syncExitUnknown)
	}
}

// Every arm of the lookup switch, because a wrong route is invisible in exactly
// two directions: it either reports a live row as unreachable (a red build over
// nothing) or an id that resolves nowhere as ok (a green build over a broken
// reference). Three of the four arms had no test at all, and the harness carried
// agent and secret routes nothing populated.
//
// The pipeline folder is the vehicle because its markers cover every kind in one
// file, and each kind is asserted on BOTH sides of its own route.
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

// A folder-level finding SCORES like a broken edge, so it has to be COUNTED like
// one. It was not: a used-but-undeclared table exited 1 saying "(0 unresolved, 0
// unreachable)" — a self-contradicting sentence, on the one line a CI job
// prints.
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

// The dependency leg needs no server, so a SIGNED-OUT run must still report it —
// which this file's own comment, the README and the customer guide all promise.
// The workflow leg returned `no_credential` before reaching it, so the one kind
// a CI job under a rotated token hits first reported nothing at all.
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

	// NO --stack: with one, an unresolvable organization lands on the
	// stack_unverified path instead, which the test below covers. Signed out, the
	// stack still matches on the instance alone, so the folder is bound and this
	// is genuinely the no-credential path.
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

// The same promise on the other unverifiable path: --stack names a stack this
// folder declares for an organization we cannot confirm is ours. Every REMOTE
// leg is unsafe there — an id lookup would ask our organization about another
// one's rows and call every one unreachable — but the dependency leg compares
// the committed file against itself and is answerable regardless.
func TestSyncCheckUnverifiedOrganizationStillReportsDependencies(t *testing.T) {
	f := newFakeInstance(t)
	// The organization lookup fails, so nothing can confirm whose stack this is.
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
	// And no id of ours was asked about another organization's rows.
	if n := requestsTo(f, "/feature/model/"); n != 0 {
		t.Errorf("%d id lookups against an organization we could not confirm", n)
	}
}

// The literal scan's prefixes come from markers.IDPrefixes(), not from a second
// hand-kept list — so a kind added to markers cannot silently stop being
// covered here. Asserted by driving every prefix the package declares through
// the real command.
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
